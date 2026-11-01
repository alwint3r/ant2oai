package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

func translateRequest(request messagesRequest, cfg config) (completionRequest, error) {
	var result completionRequest
	if strings.TrimSpace(request.Model) == "" || len(request.Model) > 256 {
		return result, errors.New("model must be a non-empty ID of at most 256 bytes")
	}
	if request.MaxTokens <= 0 {
		return result, errors.New("max_tokens must be positive")
	}
	if len(request.Messages) == 0 {
		return result, errors.New("messages must contain at least one message")
	}
	if request.Messages[len(request.Messages)-1].Role == "assistant" {
		return result, errors.New("assistant prefill is unsupported; " +
			"end messages with user content or tool results")
	}

	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return result, errors.New("temperature must be between 0 and 1")
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return result, errors.New("top_p must be between 0 and 1")
	}
	if request.TopK != nil {
		return result, errors.New("top_k has no Chat Completions equivalent")
	}
	if len(request.StopSequences) > 4 {
		return result, errors.New("Chat Completions accepts at most four stop_sequences")
	}
	for _, sequence := range request.StopSequences {
		if sequence == "" {
			return result, errors.New("stop_sequences must contain non-empty strings")
		}
	}

	if hasValue(request.ContextManagement) {
		return result, errors.New("context_management: Extra inputs are not permitted; " +
			"set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1")
	}
	if output := request.OutputConfig; output != nil {
		if hasValue(output.Format) || hasValue(output.TaskBudget) {
			return result, errors.New("output_config format and task_budget are unsupported; " +
				"set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1")
		}
	}
	if request.ServiceTier != "" && request.ServiceTier != "auto" {
		return result, errors.New("only service_tier auto is supported")
	}

	result.Model = cfg.upstreamModel(request.Model)
	result.Stream = request.Stream
	if cfg.tokenLimitField == "max_completion_tokens" {
		result.MaxCompletionTokens = request.MaxTokens
	} else {
		result.MaxTokens = request.MaxTokens
	}
	if !cfg.omitSampling {
		result.Temperature = request.Temperature
		result.TopP = request.TopP
	}
	result.Stop = request.StopSequences
	if request.Stream && cfg.streamUsage {
		result.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	toolNames := make(map[string]bool, len(request.Tools))
	for index, tool := range request.Tools {
		if tool.DeferLoading {
			return result, errors.New("defer_loading requires Anthropic tool search; set ENABLE_TOOL_SEARCH=false")
		}
		if tool.Type != "" && tool.Type != "custom" {
			return result, errors.New("server-managed tools are unsupported; " +
				"use client-side tools and set ENABLE_TOOL_SEARCH=false")
		}
		if !validToolName(tool.Name) || toolNames[tool.Name] {
			return result, fmt.Errorf("tools[%d] requires a unique function name: "+
				"1-64 ASCII letters, digits, underscores, or dashes", index)
		}
		if !isJSONObject(tool.InputSchema) {
			return result, fmt.Errorf("tools[%d].input_schema must be a JSON object", index)
		}
		toolNames[tool.Name] = true
		result.Tools = append(result.Tools, functionTool{
			Type: "function",
			Function: functionDefinition{
				Name: tool.Name, Description: tool.Description,
				Parameters: tool.InputSchema, Strict: tool.Strict,
			},
		})
	}

	if choice := request.ToolChoice; choice != nil {
		switch choice.Type {
		case "auto", "none":
			result.ToolChoice = choice.Type
		case "any":
			if len(result.Tools) == 0 {
				return result, errors.New("tool_choice any requires tools")
			}
			result.ToolChoice = "required"
		case "tool":
			if !toolNames[choice.Name] {
				return result, errors.New("tool_choice must name a declared tool")
			}
			result.ToolChoice = map[string]any{
				"type": "function", "function": map[string]string{"name": choice.Name},
			}
		default:
			return result, errors.New("tool_choice type must be auto, none, any, or tool")
		}
		if choice.DisableParallelToolUse != nil && len(result.Tools) > 0 {
			allowParallel := !*choice.DisableParallelToolUse
			result.ParallelToolCalls = &allowParallel
		}
	}

	if hasValue(request.System) {
		blocks, err := parseContent(request.System)
		if err != nil {
			return result, fmt.Errorf("system: %w", err)
		}
		var texts []string
		for _, block := range blocks {
			if block.Type != "text" || block.Text == nil {
				return result, errors.New("system must contain only text blocks")
			}
			texts = append(texts, *block.Text)
		}
		result.Messages = append(result.Messages, chatMessage{Role: "system", Content: strings.Join(texts, "\n\n")})
	}

	pendingCalls := make(map[string]bool)
	seenCallIDs := make(map[string]bool)
	var pendingAttachments []chatPart
	for index, message := range request.Messages {
		if output := message.OutputConfig; output != nil {
			if hasValue(output.Format) || hasValue(output.TaskBudget) {
				return result, fmt.Errorf("messages[%d].output_config format and task_budget are unsupported", index)
			}
		}
		blocks, err := parseContent(message.Content)
		if err != nil {
			return result, fmt.Errorf("messages[%d]: %w", index, err)
		}
		switch message.Role {
		case "assistant":
			if len(pendingCalls) != 0 {
				return result, errors.New("every tool_use must receive a tool_result before the next assistant message")
			}

			assistant := chatMessage{Role: "assistant"}
			var texts []string
			for _, block := range blocks {
				switch block.Type {
				case "text":
					if block.Text == nil {
						return result, errors.New("text blocks require a text string")
					}
					texts = append(texts, *block.Text)
				case "tool_use":
					if block.ID == "" || len(block.ID) > 256 || seenCallIDs[block.ID] {
						return result, errors.New("tool_use requires a unique ID of at most 256 bytes")
					}
					if !validToolName(block.Name) || !isJSONObject(block.Input) {
						return result, errors.New("tool_use requires an OpenAI function name and a JSON object input")
					}
					if len(assistant.ToolCalls) >= maxToolCalls {
						return result, errors.New("a message exceeds the limit of 128 tool calls")
					}
					seenCallIDs[block.ID] = true
					pendingCalls[block.ID] = true
					assistant.ToolCalls = append(assistant.ToolCalls, toolCall{
						ID: block.ID, Type: "function",
						Function: functionCall{Name: block.Name, Arguments: string(block.Input)},
					})
				case "thinking", "redacted_thinking":
					// Anthropic thinking signatures cannot be reused by this upstream.
				default:
					return result, errors.New("unsupported assistant content block")
				}
			}

			if len(texts) > 0 {
				assistant.Content = strings.Join(texts, "\n\n")
			}
			if len(texts) > 0 || len(assistant.ToolCalls) > 0 {
				result.Messages = append(result.Messages, assistant)
			}
		case "user", "system":
			var parts []chatPart
			for _, block := range blocks {
				if block.Type == "tool_result" {
					if message.Role != "user" || !pendingCalls[block.ToolUseID] {
						return result, errors.New("tool_result must match an unanswered tool_use in a user message")
					}
					text, attachments, err := translateToolResult(block)
					if err != nil {
						return result, fmt.Errorf("messages[%d].tool_result: %w", index, err)
					}
					delete(pendingCalls, block.ToolUseID)
					result.Messages = append(result.Messages, chatMessage{
						Role: "tool", ToolCallID: block.ToolUseID, Content: text,
					})
					pendingAttachments = append(pendingAttachments, attachments...)
					continue
				}
				part, err := translateUserBlock(block, message.Role)
				if err != nil {
					return result, fmt.Errorf("messages[%d]: %w", index, err)
				}
				parts = append(parts, part)
			}

			// OpenAI requires all parallel tool replies before the next user turn.
			if len(parts) > 0 && len(pendingCalls) != 0 {
				return result, errors.New("all parallel tool results must precede the next user or system content")
			}
			if len(pendingCalls) == 0 && len(pendingAttachments) > 0 {
				parts = append(pendingAttachments, parts...)
				pendingAttachments = nil
			}
			if len(parts) > 0 {
				result.Messages = append(result.Messages, chatMessage{Role: message.Role, Content: partsContent(parts)})
			}
		default:
			return result, fmt.Errorf("messages[%d].role must be user, assistant, or system", index)
		}
	}

	if len(pendingCalls) != 0 {
		return result, errors.New("messages end before all tool results are supplied")
	}
	if len(result.Messages) == 0 || result.Messages[len(result.Messages)-1].Role == "assistant" {
		return result, errors.New("messages must end with user content, system content, or tool results")
	}
	return result, nil
}

func parseContent(raw json.RawMessage) ([]inputBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("content is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, errors.New("content must be a string or an array of content blocks")
		}
		return []inputBlock{{Type: "text", Text: &text}}, nil
	}
	if raw[0] != '[' {
		return nil, errors.New("content must be a string or an array of content blocks")
	}
	var blocks []inputBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("invalid content block structure")
	}
	return blocks, nil
}

func translateUserBlock(block inputBlock, role string) (chatPart, error) {
	switch block.Type {
	case "text":
		if block.Text == nil {
			return chatPart{}, errors.New("text blocks require a text string")
		}
		return chatPart{Type: "text", Text: block.Text}, nil
	case "image":
		if role != "user" || block.Source == nil {
			return chatPart{}, errors.New("images require a user message and a source")
		}

		source := block.Source
		var imageURL string
		switch source.Type {
		case "base64":
			switch source.MediaType {
			case "image/jpeg", "image/png", "image/gif", "image/webp":
			default:
				return chatPart{}, errors.New("image media_type must be image/jpeg, image/png, image/gif, or image/webp")
			}
			if source.Data == "" {
				return chatPart{}, errors.New("base64 images require data")
			}

			decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(source.Data))
			if _, err := io.Copy(io.Discard, decoder); err != nil {
				return chatPart{}, errors.New("image data must be valid base64")
			}
			imageURL = "data:" + source.MediaType + ";base64," + source.Data
		case "url":
			parsed, err := url.Parse(source.URL)
			if err != nil || parsed.Hostname() == "" || parsed.User != nil {
				return chatPart{}, errors.New("image URLs require an absolute URL without embedded credentials")
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return chatPart{}, errors.New("image URLs must use HTTP or HTTPS")
			}
			imageURL = source.URL
		default:
			return chatPart{}, errors.New("image source type must be base64 or url")
		}
		return chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageURL}}, nil
	case "tool_reference":
		return chatPart{}, errors.New("tool_reference requires Anthropic tool search; " +
			"set ENABLE_TOOL_SEARCH=false and start a new session")
	default:
		return chatPart{}, errors.New("unsupported content block; " +
			"supported user blocks are text, images, and tool results")
	}
}

func translateToolResult(block inputBlock) (string, []chatPart, error) {
	var texts []string
	var attachments []chatPart
	if hasValue(block.Content) {
		blocks, err := parseContent(block.Content)
		if err != nil {
			return "", nil, err
		}
		for _, content := range blocks {
			part, err := translateUserBlock(content, "user")
			if err != nil {
				return "", nil, err
			}
			if part.Type == "text" {
				texts = append(texts, *part.Text)
			} else {
				// Tool messages accept text. Move images to a user message after
				// every pending tool reply and retain an explicit association.
				texts = append(texts, "[Tool result image attached in the following user message.]")
				label := "Image from tool result " + block.ToolUseID + ":"
				attachments = append(attachments, chatPart{Type: "text", Text: &label}, part)
			}
		}
	}

	text := strings.Join(texts, "\n\n")
	if block.IsError {
		text = "Tool error: " + text
	}
	return text, attachments, nil
}

func partsContent(parts []chatPart) any {
	var texts []string
	for _, part := range parts {
		if part.Type != "text" {
			return parts
		}
		texts = append(texts, *part.Text)
	}
	return strings.Join(texts, "\n\n")
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, character := range name {
		isLetter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '_' && character != '-' {
			return false
		}
	}
	return true
}
