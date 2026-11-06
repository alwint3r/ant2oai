package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	Reasoning         *responsesReasoning  `json:"reasoning,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
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
	Filename string  `json:"filename,omitempty"`
	FileData string  `json:"file_data,omitempty"`
}

// Start with the validated Chat Completions translation, which already checks
// tool history and moves tool-result attachments after all pending tool replies.
func translateResponsesRequest(input completionRequest) (responsesRequest, error) {
	result := responsesRequest{
		Model: input.Model, Store: false, Stream: true, Input: make([]responsesInputItem, 0),
		ToolChoice: input.ToolChoice, ParallelToolCalls: input.ParallelToolCalls,
	}

	// The direct route has no stop parameter. Buffered Messages replies apply
	// stops locally after the upstream stream has completed and passed validation.
	if len(input.Stop) != 0 && input.Stream {
		return result, errors.New("stop_sequences require stream: false in ChatGPT OAuth mode; " +
			"streaming stop sequences are unsupported")
	}

	if input.ReasoningEffort != "" {
		result.Reasoning = &responsesReasoning{Effort: input.ReasoningEffort}
	}
	if len(input.Tools) != 0 {
		// The direct ChatGPT route accepts flat functions through this developer
		// input item. Named tool selection keeps the original Claude tool names.
		result.Input = append(result.Input, responsesInputItem{
			Type: "additional_tools", Role: "developer",
			Tools: translateResponsesTools(input.Tools),
		})
	}
	if choice, named := input.ToolChoice.(map[string]any); named {
		function := choice["function"].(map[string]string)
		result.ToolChoice = map[string]string{"type": "function", "name": function["name"]}
	}

	for _, message := range input.Messages {
		items, err := translateResponsesMessageItems(message)
		if err != nil {
			return result, err
		}
		result.Input = append(result.Input, items...)
	}
	return result, nil
}

func translateResponsesTools(tools []functionTool) []responsesFunctionTool {
	translated := make([]responsesFunctionTool, 0, len(tools))
	for _, tool := range tools {
		definition := tool.Function
		if definition.Strict == nil {
			strict := false
			definition.Strict = &strict
		}
		translated = append(translated,
			responsesFunctionTool{Type: "function", functionDefinition: definition})
	}
	return translated
}

// translateResponsesMessageItems returns the input items of one chat message.
// The Responses protocol has separate items for text and for each tool call.
func translateResponsesMessageItems(message chatMessage) ([]responsesInputItem, error) {
	if message.Role == "tool" {
		text := message.Content.(string)
		return []responsesInputItem{{
			Type: "function_call_output", CallID: message.ToolCallID, Output: &text,
		}}, nil
	}

	var items []responsesInputItem
	if message.Content != nil {
		role := message.Role
		if role == "system" {
			role = "developer"
		}
		content, err := translateResponsesContent(message.Content)
		if err != nil {
			return nil, err
		}
		items = append(items, responsesInputItem{Type: "message", Role: role, Content: content})
	}
	for _, call := range message.ToolCalls {
		items = append(items, responsesInputItem{
			Type: "function_call", CallID: call.ID, Name: call.Function.Name,
			Arguments: call.Function.Arguments,
		})
	}
	return items, nil
}

func translateResponsesContent(content any) (any, error) {
	parts, isMultipart := content.([]chatPart)
	if !isMultipart {
		return content, nil
	}

	converted := make([]responsesInputPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			converted = append(converted, responsesInputPart{Type: "input_text", Text: part.Text})
		case "image_url":
			converted = append(converted, responsesInputPart{
				Type: "input_image", ImageURL: part.ImageURL.URL, Detail: "auto",
			})
		case "file":
			converted = append(converted, responsesInputPart{
				Type: "input_file", Filename: part.File.Filename, FileData: part.File.FileData,
			})
		default:
			return nil, errors.New("unsupported translated Responses content part")
		}
	}
	return converted, nil
}

func translateResponsesMessage(
	response completionResponse, request messagesRequest, messageID string,
) (messageResponse, error) {
	// Validate the full reply before trimming, including any tool calls after
	// a stop marker.
	result, err := translateResponse(response, request, messageID)
	if err != nil || len(request.StopSequences) == 0 {
		return result, err
	}

	// Refusals retain their full text and stop reason.
	message := response.Choices[0].Message
	if message.Content == nil || message.Refusal != "" || *result.StopReason == "refusal" {
		return result, nil
	}

	// The marker that appears first in the text wins, whatever the list order is.
	text := *message.Content
	stopAt := len(text)
	matched := ""
	for _, sequence := range request.StopSequences {
		position := strings.Index(text, sequence)
		if position >= 0 && (matched == "" || position < stopAt) {
			stopAt = position
			matched = sequence
		}
	}
	if matched == "" {
		return result, nil
	}

	// The translated reply places text before tool calls. Everything from the
	// first marker onward is omitted. Usage keeps the full upstream token counts.
	truncated := text[:stopAt]
	stopReason := "stop_sequence"
	result.Content = []outputBlock{{Type: "text", Text: &truncated}}
	result.StopReason = &stopReason
	result.StopSequence = &matched
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
	return &responsesStream{
		stream: newStreamTranslator(tools),
		items:  make(map[int]*responsesItemState), seenIDs: make(map[string]bool),
	}
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

	switch event.Type {
	case "error", "response.failed":
		return nil, responsesFailure(event)
	case "response.created":
		return nil, translator.recordCreated(event)
	case "response.output_item.added":
		return translator.startItem(event)
	case "response.output_text.delta", "response.refusal.delta":
		return translator.consumeTextDelta(event)
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return translator.consumeArguments(event)
	case "response.output_item.done":
		return translator.finishItem(event)
	case "response.completed", "response.incomplete":
		return translator.completeResponse(event)
	}

	// Content-part lifecycle, reasoning, and annotation events have no separate
	// Anthropic representation. The final output snapshot is checked below.
	return nil, nil
}

func responsesFailure(event responsesEvent) error {
	code := event.Code
	if event.Response != nil && event.Response.Error != nil {
		code = event.Response.Error.Code
	}

	message, known := chatGPTFailureMessage(code)
	if known && !strings.Contains(message, code) {
		message = code + ": " + message
	}
	return errors.New(message)
}

func (translator *responsesStream) recordCreated(event responsesEvent) error {
	if event.Response == nil || event.Response.ID == "" ||
		len(event.Response.ID) > maxIDBytes || translator.responseID != "" {
		return errors.New("upstream returned invalid Responses creation metadata")
	}
	translator.responseID = event.Response.ID
	return nil
}

func (translator *responsesStream) startItem(event responsesEvent) ([]messageEvent, error) {
	if event.OutputIndex == nil || *event.OutputIndex != len(translator.items) ||
		len(translator.items) >= maxContentBlocks || event.Item == nil ||
		event.Item.ID == "" || len(event.Item.ID) > maxIDBytes ||
		translator.seenIDs[event.Item.ID] {
		return nil, errors.New("upstream returned invalid Responses output metadata")
	}

	item := *event.Item
	state := &responsesItemState{item: item, text: make(map[int]*responsesTextState)}
	switch item.Type {
	case "function_call":
		if translator.toolCount >= maxToolCalls || item.CallID == "" ||
			len(item.CallID) > maxIDBytes || !translator.stream.declaredTools[item.Name] ||
			item.Namespace != "" {
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
	return nil, nil
}

func (translator *responsesStream) consumeTextDelta(event responsesEvent) ([]messageEvent, error) {
	state, err := translator.eventItem(event)
	if err != nil || state.item.Type != "message" || event.Delta == nil ||
		event.ContentIndex == nil || *event.ContentIndex < 0 ||
		*event.ContentIndex >= maxContentBlocks {
		return nil, errors.New("upstream returned invalid Responses text metadata")
	}

	kind := "output_text"
	if event.Type == "response.refusal.delta" {
		kind = "refusal"
	}
	return translator.addText(state, *event.ContentIndex, kind, *event.Delta)
}

func (translator *responsesStream) consumeArguments(event responsesEvent) ([]messageEvent, error) {
	state, err := translator.eventItem(event)
	if err != nil || state.item.Type != "function_call" {
		return nil, errors.New("upstream returned arguments for an unknown Responses tool")
	}

	if event.Type == "response.function_call_arguments.done" && event.Arguments != nil {
		return translator.completeArguments(state, *event.Arguments)
	}
	if event.Type != "response.function_call_arguments.delta" || event.Delta == nil ||
		state.isToolDone {
		return nil, errors.New("upstream returned an invalid Responses argument event")
	}
	return translator.addArguments(state, *event.Delta, false)
}

func (translator *responsesStream) finishItem(event responsesEvent) ([]messageEvent, error) {
	if event.OutputIndex == nil || event.Item == nil {
		return nil, errors.New("upstream returned incomplete Responses output metadata")
	}
	events, err := translator.completeItem(*event.OutputIndex, *event.Item)
	if err != nil {
		return nil, err
	}

	// Retain the verified snapshot for terminal events with an empty output array.
	state := translator.items[*event.OutputIndex]
	state.item = *event.Item
	state.isDone = true
	return events, nil
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
	return translator.stream.consume(completionResponse{
		Choices: []completionChoice{{Delta: delta}},
	})
}

func (translator *responsesStream) completeItem(
	index int, item responsesOutputItem,
) ([]messageEvent, error) {
	state := translator.items[index]
	if state == nil || item.ID != state.item.ID || item.Type != state.item.Type {
		return nil, errors.New("upstream changed Responses output metadata")
	}

	if item.Type == "function_call" {
		if item.CallID != state.item.CallID || item.Name != state.item.Name ||
			item.Namespace != "" {
			return nil, errors.New("upstream changed Responses tool metadata")
		}
		return translator.completeArguments(state, item.Arguments)
	}
	if item.Type != "message" {
		return nil, nil
	}
	return translator.completeMessage(state, item)
}

// completeMessage compares a final message with its streamed text. Text that was
// not streamed is emitted now, and streamed text must match the final snapshot.
func (translator *responsesStream) completeMessage(
	state *responsesItemState, item responsesOutputItem,
) ([]messageEvent, error) {
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
		if part.kind != content.Type || part.bytes != len(text) ||
			!bytes.Equal(part.digest.Sum(nil), digest[:]) {
			return nil, errors.New("upstream changed Responses text after streaming it")
		}
	}

	if len(state.text) != len(item.Content) {
		return nil, errors.New("upstream Responses final content " +
			"does not match its streamed content")
	}
	return events, nil
}

func (translator *responsesStream) completeResponse(event responsesEvent) ([]messageEvent, error) {
	response := event.Response
	if response == nil {
		return nil, errors.New("upstream Responses completion is missing its response object")
	}
	if translator.responseID == "" {
		return nil, errors.New("upstream Responses completion arrived before response.created")
	}
	if response.ID == "" || response.ID != translator.responseID {
		return nil, errors.New("upstream Responses completion ID does not match response.created")
	}
	if response.Error != nil {
		message, _ := chatGPTFailureMessage(response.Error.Code)
		return nil, errors.New(message)
	}

	output, err := translator.finalOutput(response.Output)
	if err != nil {
		return nil, err
	}
	finish, err := responsesFinishReason(event)
	if err != nil {
		return nil, err
	}

	// Check every item again, and collect the reply for buffered clients.
	var events []messageEvent
	var text, refusal strings.Builder
	message := completionMessage{Role: "assistant"}
	for index, item := range output {
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

	completion := completionResponse{
		Choices: []completionChoice{{Message: message, FinishReason: &finish}},
	}
	if response.Usage != nil {
		completion.Usage = &completionUsage{
			PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens,
		}
		cachedTokens := response.Usage.InputTokensDetails.CachedTokens
		completion.Usage.PromptTokensDetails.CachedTokens = cachedTokens
	}

	additional, err := translator.stream.consume(completion)
	if err != nil {
		return nil, err
	}
	events = append(events, additional...)
	translator.final = &completion
	return events, nil
}

// finalOutput returns the output items of a terminal event. The direct route can
// send an empty array. Recover the output only from item-done snapshots that
// were already checked against the deltas.
func (translator *responsesStream) finalOutput(
	output []responsesOutputItem,
) ([]responsesOutputItem, error) {
	if output != nil && len(output) == 0 && len(translator.items) > 0 {
		output = make([]responsesOutputItem, len(translator.items))
		for index := range output {
			state := translator.items[index]
			if !state.isDone {
				return nil, errors.New("upstream Responses completion has empty output " +
					"before all streamed items completed")
			}
			output[index] = state.item
		}
	}

	if len(output) != len(translator.items) {
		return nil, fmt.Errorf("upstream Responses completion output count mismatch: "+
			"streamed=%d final=%d", len(translator.items), len(output))
	}
	return output, nil
}

func responsesFinishReason(event responsesEvent) (string, error) {
	response := event.Response
	if event.Type == "response.incomplete" && response.Status == "incomplete" &&
		response.IncompleteDetails != nil {
		switch response.IncompleteDetails.Reason {
		case "max_output_tokens":
			return "length", nil
		case "content_filter":
			return "content_filter", nil
		default:
			return "", errors.New("upstream Responses request ended " +
				"with an unsupported incomplete reason")
		}
	}

	if event.Type != "response.completed" || response.Status != "completed" {
		return "", errors.New("upstream Responses request did not complete successfully")
	}
	return "stop", nil
}

func (translator *responsesStream) finish() ([]messageEvent, error) {
	if translator.final == nil {
		return nil, errors.New("upstream Responses stream ended before response completion")
	}
	return translator.stream.finish()
}
