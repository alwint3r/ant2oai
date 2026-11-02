package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"hash"
	"strings"
)

type responsesRequest struct {
	Model             string               `json:"model"`
	Input             []responsesInputItem `json:"input"`
	Store             bool                 `json:"store"`
	Stream            bool                 `json:"stream"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
}

type responsesInputItem struct {
	Type      string                  `json:"type"`
	Role      string                  `json:"role,omitempty"`
	Content   any                     `json:"content,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"`
	Output    *string                 `json:"output,omitempty"`
	Tools     []responsesFunctionTool `json:"tools,omitempty"`
}

type responsesFunctionTool struct {
	Type string `json:"type"`
	functionDefinition
}

type responsesInputPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

// Start with the validated Chat Completions translation, which already checks
// tool history and moves tool-result images after all pending tool replies.
func translateResponsesRequest(input completionRequest) (responsesRequest, error) {
	result := responsesRequest{Model: input.Model, Store: false, Stream: true,
		Input: make([]responsesInputItem, 0), ToolChoice: input.ToolChoice,
		ParallelToolCalls: input.ParallelToolCalls}
	if len(input.Stop) != 0 {
		return result, errors.New("stop_sequences are unsupported in ChatGPT OAuth mode")
	}
	if len(input.Tools) != 0 {
		tools := make([]responsesFunctionTool, 0, len(input.Tools))
		for _, tool := range input.Tools {
			definition := tool.Function
			if definition.Strict == nil {
				strict := false
				definition.Strict = &strict
			}
			tools = append(tools, responsesFunctionTool{Type: "function", functionDefinition: definition})
		}
		// The direct ChatGPT route accepts flat functions through this developer
		// input item. Named tool selection keeps the original Claude tool names.
		result.Input = append(result.Input, responsesInputItem{
			Type: "additional_tools", Role: "developer", Tools: tools,
		})
	}
	if choice, named := input.ToolChoice.(map[string]any); named {
		function := choice["function"].(map[string]string)
		result.ToolChoice = map[string]string{"type": "function", "name": function["name"]}
	}

	for _, message := range input.Messages {
		if message.Role == "tool" {
			text := message.Content.(string)
			result.Input = append(result.Input, responsesInputItem{
				Type: "function_call_output", CallID: message.ToolCallID, Output: &text,
			})
			continue
		}
		if message.Content != nil {
			role := message.Role
			if role == "system" {
				role = "developer"
			}
			content := message.Content
			if parts, isMultipart := content.([]chatPart); isMultipart {
				converted := make([]responsesInputPart, 0, len(parts))
				for _, part := range parts {
					if part.Type == "text" {
						converted = append(converted, responsesInputPart{Type: "input_text", Text: part.Text})
					} else {
						converted = append(converted, responsesInputPart{
							Type: "input_image", ImageURL: part.ImageURL.URL, Detail: "auto",
						})
					}
				}
				content = converted
			}
			result.Input = append(result.Input, responsesInputItem{Type: "message", Role: role, Content: content})
		}
		for _, call := range message.ToolCalls {
			result.Input = append(result.Input, responsesInputItem{
				Type: "function_call", CallID: call.ID, Name: call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}
	return result, nil
}

type responsesPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

type responsesOutputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"`
	Content   []responsesPart `json:"content"`
}

type responsesResult struct {
	ID                string                `json:"id"`
	Status            string                `json:"status"`
	Output            []responsesOutputItem `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
	Usage *struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type responsesEvent struct {
	Type         string               `json:"type"`
	OutputIndex  *int                 `json:"output_index"`
	ContentIndex *int                 `json:"content_index"`
	ItemID       string               `json:"item_id"`
	Item         *responsesOutputItem `json:"item"`
	Delta        *string              `json:"delta"`
	Arguments    *string              `json:"arguments"`
	Response     *responsesResult     `json:"response"`
	Code         string               `json:"code"`
}

type responsesTextState struct {
	kind   string
	digest hash.Hash
	bytes  int
}

type responsesItemState struct {
	item       responsesOutputItem
	toolIndex  int
	arguments  hash.Hash
	bytes      int
	isToolDone bool
	isDone     bool
	text       map[int]*responsesTextState
}

type responsesStream struct {
	stream     *streamTranslator
	items      map[int]*responsesItemState
	seenIDs    map[string]bool
	toolCount  int
	responseID string
	final      *completionResponse
}

func newResponsesStream(tools []inputTool) *responsesStream {
	return &responsesStream{stream: newStreamTranslator(tools),
		items: make(map[int]*responsesItemState), seenIDs: make(map[string]bool)}
}

func (translator *responsesStream) consume(data []byte, eventName string) ([]messageEvent, error) {
	var event responsesEvent
	if checkJSONDepth(data) != nil || json.Unmarshal(data, &event) != nil || event.Type == "" {
		return nil, errors.New("upstream sent an invalid Responses stream event")
	}
	if eventName != "" && eventName != "message" && eventName != event.Type {
		return nil, errors.New("upstream Responses event name does not match its data")
	}
	if translator.final != nil {
		return nil, errors.New("upstream sent events after response completion")
	}
	if event.Type == "error" || event.Type == "response.failed" {
		code := event.Code
		if event.Response != nil && event.Response.Error != nil {
			code = event.Response.Error.Code
		}
		message, known := chatGPTFailureMessage(code)
		if known && !strings.Contains(message, code) {
			message = code + ": " + message
		}
		return nil, errors.New(message)
	}

	switch event.Type {
	case "response.created":
		if event.Response == nil || event.Response.ID == "" ||
			len(event.Response.ID) > 256 || translator.responseID != "" {
			return nil, errors.New("upstream returned invalid Responses creation metadata")
		}
		translator.responseID = event.Response.ID
	case "response.output_item.added":
		if event.OutputIndex == nil || *event.OutputIndex != len(translator.items) ||
			len(translator.items) >= maxContentBlocks || event.Item == nil ||
			event.Item.ID == "" || len(event.Item.ID) > 256 || translator.seenIDs[event.Item.ID] {
			return nil, errors.New("upstream returned invalid Responses output metadata")
		}

		item := *event.Item
		state := &responsesItemState{item: item, text: make(map[int]*responsesTextState)}
		switch item.Type {
		case "function_call":
			if translator.toolCount >= maxToolCalls || item.CallID == "" || len(item.CallID) > 256 ||
				!translator.stream.declaredTools[item.Name] || item.Namespace != "" {
				return nil, errors.New("upstream returned an unsupported or undeclared Responses tool")
			}
			state.toolIndex = translator.toolCount
			state.arguments = sha256.New()
			translator.toolCount++
		case "message":
			if item.Role != "assistant" {
				return nil, errors.New("upstream returned an unsupported Responses message role")
			}
		case "reasoning":
			// Claude history has no portable field for OpenAI reasoning state.
		default:
			return nil, errors.New("upstream returned an unsupported Responses output item")
		}

		translator.items[*event.OutputIndex] = state
		translator.seenIDs[item.ID] = true
		if item.Type == "function_call" {
			return translator.addArguments(state, item.Arguments, true)
		}
	case "response.output_text.delta", "response.refusal.delta":
		state, err := translator.eventItem(event)
		if err != nil || state.item.Type != "message" || event.Delta == nil ||
			event.ContentIndex == nil || *event.ContentIndex < 0 || *event.ContentIndex >= maxContentBlocks {
			return nil, errors.New("upstream returned invalid Responses text metadata")
		}
		kind := "output_text"
		if event.Type == "response.refusal.delta" {
			kind = "refusal"
		}
		return translator.addText(state, *event.ContentIndex, kind, *event.Delta)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		state, err := translator.eventItem(event)
		if err != nil || state.item.Type != "function_call" {
			return nil, errors.New("upstream returned arguments for an unknown Responses tool")
		}
		if event.Type == "response.function_call_arguments.done" && event.Arguments != nil {
			return translator.completeArguments(state, *event.Arguments)
		}
		if event.Type != "response.function_call_arguments.delta" || event.Delta == nil || state.isToolDone {
			return nil, errors.New("upstream returned an invalid Responses argument event")
		}
		return translator.addArguments(state, *event.Delta, false)
	case "response.output_item.done":
		if event.OutputIndex == nil || event.Item == nil {
			return nil, errors.New("upstream returned incomplete Responses output metadata")
		}
		return translator.completeItem(*event.OutputIndex, *event.Item)
	case "response.completed", "response.incomplete":
		return translator.completeResponse(event)
	}
	// Content-part lifecycle, reasoning, and annotation events have no separate
	// Anthropic representation. The final output snapshot is checked below.
	return nil, nil
}

func (translator *responsesStream) eventItem(event responsesEvent) (*responsesItemState, error) {
	if event.OutputIndex != nil {
		state := translator.items[*event.OutputIndex]
		if state != nil && state.item.ID == event.ItemID && !state.isDone {
			return state, nil
		}
	}
	return nil, errors.New("upstream Responses event references an unknown output item")
}

func (translator *responsesStream) addArguments(
	state *responsesItemState, arguments string, includeMetadata bool,
) ([]messageEvent, error) {
	if state.bytes+len(arguments) > maxToolArguments {
		return nil, errors.New("upstream tool arguments exceed 8 MiB")
	}
	state.arguments.Write([]byte(arguments))
	state.bytes += len(arguments)
	delta := toolCallDelta{Index: &state.toolIndex, Function: functionCall{Arguments: arguments}}
	if includeMetadata {
		delta.ID, delta.Type, delta.Function.Name = state.item.CallID, "function", state.item.Name
	}
	return translator.stream.consume(completionResponse{Choices: []completionChoice{{
		Delta: completionDelta{ToolCalls: []toolCallDelta{delta}},
	}}})
}

func (translator *responsesStream) completeArguments(
	state *responsesItemState, arguments string,
) ([]messageEvent, error) {
	var events []messageEvent
	if state.bytes == 0 && arguments != "" && !state.isToolDone {
		var err error
		events, err = translator.addArguments(state, arguments, false)
		if err != nil {
			return nil, err
		}
	}
	digest := sha256.Sum256([]byte(arguments))
	if len(arguments) != state.bytes || !bytes.Equal(state.arguments.Sum(nil), digest[:]) {
		return nil, errors.New("upstream changed Responses tool arguments after streaming them")
	}
	state.isToolDone = true
	return events, nil
}

func (translator *responsesStream) addText(
	state *responsesItemState, index int, kind, text string,
) ([]messageEvent, error) {
	part := state.text[index]
	if part == nil {
		part = &responsesTextState{kind: kind, digest: sha256.New()}
		state.text[index] = part
	}
	if part.kind != kind {
		return nil, errors.New("upstream changed the type of a Responses content part")
	}
	part.digest.Write([]byte(text))
	part.bytes += len(text)
	delta := completionDelta{Content: &text}
	if kind == "refusal" {
		delta = completionDelta{Refusal: &text}
	}
	return translator.stream.consume(completionResponse{Choices: []completionChoice{{Delta: delta}}})
}

func (translator *responsesStream) completeItem(
	index int, item responsesOutputItem,
) ([]messageEvent, error) {
	state := translator.items[index]
	if state == nil || item.ID != state.item.ID || item.Type != state.item.Type {
		return nil, errors.New("upstream changed Responses output metadata")
	}
	state.isDone = true
	if item.Type == "function_call" {
		if item.CallID != state.item.CallID || item.Name != state.item.Name || item.Namespace != "" {
			return nil, errors.New("upstream changed Responses tool metadata")
		}
		return translator.completeArguments(state, item.Arguments)
	}
	if item.Type != "message" {
		return nil, nil
	}
	if item.Role != "assistant" || len(item.Content) > maxContentBlocks {
		return nil, errors.New("upstream returned an invalid Responses message")
	}
	var events []messageEvent
	for contentIndex, content := range item.Content {
		text := content.Text
		if content.Type == "refusal" {
			text = content.Refusal
		} else if content.Type != "output_text" {
			return nil, errors.New("upstream returned unsupported Responses content")
		}
		part := state.text[contentIndex]
		if part == nil || part.bytes == 0 {
			additional, err := translator.addText(state, contentIndex, content.Type, text)
			if err != nil {
				return nil, err
			}
			events = append(events, additional...)
			part = state.text[contentIndex]
		}
		digest := sha256.Sum256([]byte(text))
		if part.kind != content.Type || part.bytes != len(text) || !bytes.Equal(part.digest.Sum(nil), digest[:]) {
			return nil, errors.New("upstream changed Responses text after streaming it")
		}
	}
	if len(state.text) != len(item.Content) {
		return nil, errors.New("upstream Responses final content does not match its streamed content")
	}
	return events, nil
}

func (translator *responsesStream) completeResponse(event responsesEvent) ([]messageEvent, error) {
	response := event.Response
	if response == nil || response.ID == "" || response.ID != translator.responseID ||
		len(response.Output) != len(translator.items) || response.Error != nil {
		return nil, errors.New("upstream returned an invalid Responses completion")
	}
	finish := "stop"
	if event.Type == "response.incomplete" && response.Status == "incomplete" &&
		response.IncompleteDetails != nil {
		switch response.IncompleteDetails.Reason {
		case "max_output_tokens":
			finish = "length"
		case "content_filter":
			finish = "content_filter"
		default:
			return nil, errors.New("upstream Responses request ended with an unsupported incomplete reason")
		}
	} else if event.Type != "response.completed" || response.Status != "completed" {
		return nil, errors.New("upstream Responses request did not complete successfully")
	}

	var events []messageEvent
	var text, refusal strings.Builder
	message := completionMessage{Role: "assistant"}
	for index, item := range response.Output {
		additional, err := translator.completeItem(index, item)
		if err != nil {
			return nil, err
		}
		events = append(events, additional...)
		if item.Type == "function_call" {
			message.ToolCalls = append(message.ToolCalls, toolCall{
				Type: "function", ID: item.CallID,
				Function: functionCall{Name: item.Name, Arguments: item.Arguments},
			})
		}
		if item.Type == "message" {
			for _, content := range item.Content {
				text.WriteString(content.Text)
				refusal.WriteString(content.Refusal)
			}
		}
	}
	joined := text.String()
	message.Content, message.Refusal = &joined, refusal.String()
	completion := completionResponse{Choices: []completionChoice{{Message: message, FinishReason: &finish}}}
	if response.Usage != nil {
		completion.Usage = &completionUsage{
			PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens,
		}
		completion.Usage.PromptTokensDetails.CachedTokens = response.Usage.InputTokensDetails.CachedTokens
	}
	additional, err := translator.stream.consume(completion)
	if err != nil {
		return nil, err
	}
	events = append(events, additional...)
	translator.final = &completion
	return events, nil
}

func (translator *responsesStream) finish() ([]messageEvent, error) {
	if translator.final == nil {
		return nil, errors.New("upstream Responses stream ended before response completion")
	}
	return translator.stream.finish()
}
