package main

import (
	"encoding/json"
	"errors"
)

func translateResponse(
	response completionResponse, request messagesRequest, messageID string,
) (messageResponse, error) {
	result := newMessage(request.Model, messageID)

	// The reply must have the shape of one complete assistant message.
	if hasValue(response.Error) || len(response.Choices) != 1 || response.Choices[0].Index != 0 {
		return result, errors.New("upstream must return exactly one completion choice")
	}
	choice := response.Choices[0]
	if choice.FinishReason == nil {
		return result, errors.New("upstream response lacks a finish reason")
	}
	if choice.Message.Role != "assistant" || hasValue(choice.Message.FunctionCall) {
		return result, errors.New("upstream returned an unsupported assistant message")
	}

	if choice.Message.Content != nil && *choice.Message.Content != "" {
		result.Content = append(result.Content,
			outputBlock{Type: "text", Text: choice.Message.Content})
	}
	if choice.Message.Refusal != "" {
		refusal := choice.Message.Refusal
		result.Content = append(result.Content, outputBlock{Type: "text", Text: &refusal})
	}

	if len(choice.Message.ToolCalls) > maxToolCalls {
		return result, errors.New("upstream exceeded the limit of 128 tool calls")
	}
	declaredTools := declaredToolNames(request.Tools)
	seenIDs := make(map[string]bool)
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" || call.ID == "" || len(call.ID) > maxIDBytes ||
			seenIDs[call.ID] {
			return result, errors.New("upstream returned invalid or duplicate tool metadata")
		}
		if !declaredTools[call.Function.Name] {
			return result, errors.New("upstream returned invalid or undeclared tool metadata")
		}
		arguments, err := toolArguments(call.Function.Arguments)
		if err != nil {
			return result, err
		}
		seenIDs[call.ID] = true
		result.Content = append(result.Content, outputBlock{
			Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: arguments,
		})
	}

	stopReason, err := translateStopReason(
		*choice.FinishReason, len(seenIDs) > 0, choice.Message.Refusal != "",
	)
	if err != nil {
		return result, err
	}
	result.StopReason = &stopReason
	result.Usage, err = translateUsage(response.Usage)
	if err != nil {
		return result, err
	}
	if len(result.Content) == 0 {
		empty := ""
		result.Content = append(result.Content, outputBlock{Type: "text", Text: &empty})
	}
	return result, nil
}

func newMessage(model, messageID string) messageResponse {
	return messageResponse{
		ID: messageID, Type: "message", Role: "assistant", Model: model,
		Content: make([]outputBlock, 0),
	}
}

func declaredToolNames(tools []inputTool) map[string]bool {
	names := make(map[string]bool, len(tools))
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}

func toolArguments(arguments string) (json.RawMessage, error) {
	if len(arguments) > maxToolArguments {
		return nil, errors.New("upstream tool arguments exceed 8 MiB")
	}
	if arguments == "" {
		arguments = "{}"
	}
	raw := json.RawMessage(arguments)
	if !isJSONObject(raw) || checkJSONDepth(raw) != nil {
		return nil, errors.New("upstream tool arguments must be a complete JSON object " +
			"within the nesting limit")
	}
	return raw, nil
}

func translateStopReason(reason string, hasTools, hasRefusal bool) (string, error) {
	switch reason {
	case "length":
		return "max_tokens", nil
	case "content_filter":
		return "refusal", nil
	case "tool_calls":
		if !hasTools {
			return "", errors.New("upstream finished with tool_calls without any tool calls")
		}
		return "tool_use", nil
	case "stop":
		if hasTools {
			return "tool_use", nil
		}
		if hasRefusal {
			return "refusal", nil
		}
		// Chat Completions does not identify which stop sequence matched.
		return "end_turn", nil
	default:
		return "", errors.New("upstream returned an unsupported finish reason")
	}
}

func translateUsage(usage *completionUsage) (messageUsage, error) {
	if usage == nil {
		return messageUsage{}, nil
	}
	cachedTokens := usage.PromptTokensDetails.CachedTokens
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 ||
		cachedTokens < 0 || cachedTokens > usage.PromptTokens {
		return messageUsage{}, errors.New("upstream returned invalid token usage")
	}
	return messageUsage{
		InputTokens: usage.PromptTokens - cachedTokens, OutputTokens: usage.CompletionTokens,
		CacheReadInputTokens: cachedTokens,
	}, nil
}
