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

	// Reject unsupported input first, so that the client sees one clear error.
	if err := validateRequestOptions(request); err != nil {
		return result, err
	}
	reasoningEffort, err := parseReasoningEffort(request.OutputConfig)
	if err != nil {
		return result, err
	}
	if request.ServiceTier != "" && request.ServiceTier != "auto" {
		return result, errors.New("only service_tier auto is supported")
	}

	// Request options.
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
	if !cfg.omitReasoningEffort {
		result.ReasoningEffort = reasoningEffort
	}
	result.Stop = request.StopSequences
	if request.Stream && cfg.streamUsage {
		result.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	// Tools.
	tools, toolNames, err := translateTools(request.Tools)
	if err != nil {
		return result, err
	}
	result.Tools = tools
	if request.ToolChoice != nil {
		if err := applyToolChoice(&result, *request.ToolChoice, toolNames); err != nil {
			return result, err
		}
	}

	// Conversation.
	if hasValue(request.System) {
		system, err := translateSystem(request.System)
		if err != nil {
			return result, err
		}
		result.Messages = append(result.Messages, system)
	}
	history := newMessageHistory(result.Messages)
	for index, message := range request.Messages {
		if err := history.add(index, message); err != nil {
			return result, err
		}
	}
	result.Messages = history.messages

	if len(history.pendingCalls) != 0 {
		return result, errors.New("messages end before all tool results are supplied")
	}
	if len(result.Messages) == 0 || result.Messages[len(result.Messages)-1].Role == "assistant" {
		return result, errors.New("messages must end with user content, system content, " +
			"or tool results")
	}
	return result, nil
}

func validateRequestOptions(request messagesRequest) error {
	if strings.TrimSpace(request.Model) == "" || len(request.Model) > maxIDBytes {
		return errors.New("model must be a non-empty ID of at most 256 bytes")
	}
	if request.MaxTokens <= 0 {
		return errors.New("max_tokens must be positive")
	}
	if len(request.Messages) == 0 {
		return errors.New("messages must contain at least one message")
	}
	if request.Messages[len(request.Messages)-1].Role == "assistant" {
		return errors.New("assistant prefill is unsupported; " +
			"end messages with user content or tool results")
	}

	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return errors.New("temperature must be between 0 and 1")
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return errors.New("top_p must be between 0 and 1")
	}
	if request.TopK != nil {
		return errors.New("top_k has no Chat Completions equivalent")
	}
	if len(request.StopSequences) > maxStopSequences {
		return errors.New("Chat Completions accepts at most four stop_sequences")
	}
	for _, sequence := range request.StopSequences {
		if sequence == "" {
			return errors.New("stop_sequences must contain non-empty strings")
		}
	}

	if hasValue(request.ContextManagement) {
		return errors.New("context_management: Extra inputs are not permitted; " +
			"set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1")
	}
	return nil
}

// parseReasoningEffort returns the Claude effort level. Levels are forwarded
// unchanged as the OpenAI reasoning effort, including max. A model that lacks a
// level rejects the request.
func parseReasoningEffort(output *outputConfig) (string, error) {
	if output == nil {
		return "", nil
	}
	if hasValue(output.Format) || hasValue(output.TaskBudget) {
		return "", errors.New("output_config format and task_budget are unsupported; " +
			"set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1")
	}
	if !hasValue(output.Effort) {
		return "", nil
	}

	var effort string
	if json.Unmarshal(output.Effort, &effort) != nil {
		return "", errors.New("output_config.effort must be a string")
	}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort, nil
	default:
		return "", errors.New("output_config.effort must be low, medium, high, xhigh, or max")
	}
}

// translateTools returns the function definitions and the set of declared names.
func translateTools(tools []inputTool) ([]functionTool, map[string]bool, error) {
	var translated []functionTool
	names := make(map[string]bool, len(tools))
	for index, tool := range tools {
		if tool.DeferLoading {
			return nil, nil, errors.New("defer_loading requires Anthropic tool search; " +
				"set ENABLE_TOOL_SEARCH=false")
		}
		if tool.Type != "" && tool.Type != "custom" {
			return nil, nil, errors.New("server-managed tools are unsupported; " +
				"use client-side tools and set ENABLE_TOOL_SEARCH=false")
		}
		if !validToolName(tool.Name) || names[tool.Name] {
			return nil, nil, fmt.Errorf("tools[%d] requires a unique function name: "+
				"1-64 ASCII letters, digits, underscores, or dashes", index)
		}
		if !isJSONObject(tool.InputSchema) {
			return nil, nil, fmt.Errorf("tools[%d].input_schema must be a JSON object", index)
		}

		names[tool.Name] = true
		translated = append(translated, functionTool{
			Type: "function",
			Function: functionDefinition{
				Name: tool.Name, Description: tool.Description,
				Parameters: tool.InputSchema, Strict: tool.Strict,
			},
		})
	}
	return translated, names, nil
}

func applyToolChoice(
	result *completionRequest, choice toolChoice, toolNames map[string]bool,
) error {
	switch choice.Type {
	case "auto", "none":
		result.ToolChoice = choice.Type
	case "any":
		if len(result.Tools) == 0 {
			return errors.New("tool_choice any requires tools")
		}
		result.ToolChoice = "required"
	case "tool":
		if !toolNames[choice.Name] {
			return errors.New("tool_choice must name a declared tool")
		}
		result.ToolChoice = map[string]any{
			"type": "function", "function": map[string]string{"name": choice.Name},
		}
	default:
		return errors.New("tool_choice type must be auto, none, any, or tool")
	}

	if choice.DisableParallelToolUse != nil && len(result.Tools) > 0 {
		allowParallel := !*choice.DisableParallelToolUse
		result.ParallelToolCalls = &allowParallel
	}
	return nil
}

func translateSystem(raw json.RawMessage) (chatMessage, error) {
	blocks, err := parseContent(raw)
	if err != nil {
		return chatMessage{}, fmt.Errorf("system: %w", err)
	}

	var texts []string
	for _, block := range blocks {
		if block.Type != "text" || block.Text == nil {
			return chatMessage{}, errors.New("system must contain only text blocks")
		}
		texts = append(texts, *block.Text)
	}
	return chatMessage{Role: "system", Content: strings.Join(texts, "\n\n")}, nil
}

// messageHistory owns the state that links tool calls to their replies while the
// conversation is translated in order.
type messageHistory struct {
	messages           []chatMessage
	pendingCalls       map[string]bool
	seenCallIDs        map[string]bool
	pendingAttachments []chatPart
}

func newMessageHistory(messages []chatMessage) *messageHistory {
	return &messageHistory{
		messages: messages, pendingCalls: make(map[string]bool),
		seenCallIDs: make(map[string]bool),
	}
}

func (history *messageHistory) add(index int, message inputMessage) error {
	if output := message.OutputConfig; output != nil {
		if hasValue(output.Format) || hasValue(output.TaskBudget) {
			return fmt.Errorf("messages[%d].output_config format and task_budget are unsupported",
				index)
		}
	}
	blocks, err := parseContent(message.Content)
	if err != nil {
		return fmt.Errorf("messages[%d]: %w", index, err)
	}

	switch message.Role {
	case "assistant":
		return history.addAssistant(blocks)
	case "user", "system":
		return history.addUserOrSystem(index, message.Role, blocks)
	default:
		return fmt.Errorf("messages[%d].role must be user, assistant, or system", index)
	}
}

func (history *messageHistory) addAssistant(blocks []inputBlock) error {
	if len(history.pendingCalls) != 0 {
		return errors.New("every tool_use must receive a tool_result " +
			"before the next assistant message")
	}

	assistant := chatMessage{Role: "assistant"}
	var texts []string
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == nil {
				return errors.New("text blocks require a text string")
			}
			texts = append(texts, *block.Text)

		case "tool_use":
			if block.ID == "" || len(block.ID) > maxIDBytes || history.seenCallIDs[block.ID] {
				return errors.New("tool_use requires a unique ID of at most 256 bytes")
			}
			if !validToolName(block.Name) || !isJSONObject(block.Input) {
				return errors.New("tool_use requires an OpenAI function name " +
					"and a JSON object input")
			}
			if len(assistant.ToolCalls) >= maxToolCalls {
				return errors.New("a message exceeds the limit of 128 tool calls")
			}
			history.seenCallIDs[block.ID] = true
			history.pendingCalls[block.ID] = true
			assistant.ToolCalls = append(assistant.ToolCalls, toolCall{
				ID: block.ID, Type: "function",
				Function: functionCall{Name: block.Name, Arguments: string(block.Input)},
			})

		case "thinking", "redacted_thinking":
			// Anthropic thinking signatures cannot be reused by this upstream.

		default:
			return errors.New("unsupported assistant content block")
		}
	}

	if len(texts) > 0 {
		assistant.Content = strings.Join(texts, "\n\n")
	}
	if len(texts) > 0 || len(assistant.ToolCalls) > 0 {
		history.messages = append(history.messages, assistant)
	}
	return nil
}

func (history *messageHistory) addUserOrSystem(
	index int, role string, blocks []inputBlock,
) error {
	var parts []chatPart
	for _, block := range blocks {
		if block.Type == "tool_result" {
			if err := history.addToolResult(index, role, block); err != nil {
				return err
			}
			continue
		}
		part, err := translateUserBlock(block, role)
		if err != nil {
			return fmt.Errorf("messages[%d]: %w", index, err)
		}
		parts = append(parts, part)
	}

	// OpenAI requires all parallel tool replies before the next user turn.
	if len(parts) > 0 && len(history.pendingCalls) != 0 {
		return errors.New("all parallel tool results must precede " +
			"the next user or system content")
	}
	if len(history.pendingCalls) == 0 && len(history.pendingAttachments) > 0 {
		parts = append(history.pendingAttachments, parts...)
		history.pendingAttachments = nil
	}
	if len(parts) > 0 {
		history.messages = append(history.messages,
			chatMessage{Role: role, Content: partsContent(parts)})
	}
	return nil
}

func (history *messageHistory) addToolResult(index int, role string, block inputBlock) error {
	if role != "user" || !history.pendingCalls[block.ToolUseID] {
		return errors.New("tool_result must match an unanswered tool_use in a user message")
	}
	text, attachments, err := translateToolResult(block)
	if err != nil {
		return fmt.Errorf("messages[%d].tool_result: %w", index, err)
	}

	delete(history.pendingCalls, block.ToolUseID)
	history.messages = append(history.messages, chatMessage{
		Role: "tool", ToolCallID: block.ToolUseID, Content: text,
	})
	history.pendingAttachments = append(history.pendingAttachments, attachments...)
	return nil
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
	const supportedBlocks = "supported user blocks are text, images, PDF documents, " +
		"and tool results"

	switch block.Type {
	case "text":
		if block.Text == nil {
			return chatPart{}, errors.New("text blocks require a text string")
		}
		return chatPart{Type: "text", Text: block.Text}, nil
	case "image":
		return translateImageBlock(block, role)
	case "document":
		return translateDocumentBlock(block, role)
	case "tool_reference":
		return chatPart{}, errors.New("tool_reference requires Anthropic tool search; " +
			"set ENABLE_TOOL_SEARCH=false and start a new session")
	case "search_result", "audio", "video":
		// Only known type names are safe to include in completion logs.
		return chatPart{}, fmt.Errorf("unsupported content block type %s; %s",
			block.Type, supportedBlocks)
	default:
		return chatPart{}, errors.New("unsupported content block; " + supportedBlocks)
	}
}

func translateImageBlock(block inputBlock, role string) (chatPart, error) {
	if role != "user" || block.Source == nil {
		return chatPart{}, errors.New("images require a user message and a source")
	}

	imageURL, err := imageSourceURL(block.Source)
	if err != nil {
		return chatPart{}, err
	}
	return chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: imageURL}}, nil
}

func imageSourceURL(source *contentSource) (string, error) {
	switch source.Type {
	case "base64":
		switch source.MediaType {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
		default:
			return "", errors.New("image media_type must be image/jpeg, image/png, " +
				"image/gif, or image/webp")
		}
		if source.Data == "" {
			return "", errors.New("base64 images require data")
		}
		if _, err := decodedBase64Size(source.Data); err != nil {
			return "", errors.New("image data must be valid base64")
		}
		return "data:" + source.MediaType + ";base64," + source.Data, nil

	case "url":
		parsed, err := url.Parse(source.URL)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil {
			return "", errors.New("image URLs require an absolute URL " +
				"without embedded credentials")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", errors.New("image URLs must use HTTP or HTTPS")
		}
		return source.URL, nil

	default:
		return "", errors.New("image source type must be base64 or url")
	}
}

func translateDocumentBlock(block inputBlock, role string) (chatPart, error) {
	if role != "user" || block.Source == nil {
		return chatPart{}, errors.New("PDF documents require a user message and a source")
	}
	// Reject document options whose meaning would be lost in this translation.
	if block.Title != "" || block.Context != "" || hasValue(block.Citations) {
		return chatPart{}, errors.New("PDF document title, context, and citations are unsupported")
	}

	source := block.Source
	if source.Type != "base64" || source.MediaType != "application/pdf" {
		return chatPart{}, errors.New("document source must be base64 " +
			"with media_type application/pdf")
	}
	if source.Data == "" {
		return chatPart{}, errors.New("base64 PDF documents require data")
	}
	decodedBytes, err := decodedBase64Size(source.Data)
	if err != nil || decodedBytes == 0 {
		return chatPart{}, errors.New("PDF document data must be non-empty valid base64")
	}

	return chatPart{Type: "file", File: &chatFile{
		Filename: "document.pdf", FileData: "data:application/pdf;base64," + source.Data,
	}}, nil
}

// decodedBase64Size validates data without keeping the decoded bytes in memory.
func decodedBase64Size(data string) (int64, error) {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	return io.Copy(io.Discard, decoder)
}

func translateToolResult(block inputBlock) (string, []chatPart, error) {
	var texts []string
	var attachments []chatPart
	if hasValue(block.Content) {
		blocks, err := parseContent(block.Content)
		if err != nil {
			return "", nil, err
		}
		for index, content := range blocks {
			part, err := translateUserBlock(content, "user")
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			if part.Type == "text" {
				texts = append(texts, *part.Text)
				continue
			}

			// Attachments follow every pending tool reply. Labels retain their
			// association with the tool call after moving to a user message.
			marker := "[Tool result image attached in the following user message.]"
			label := "Image from tool result " + block.ToolUseID + ":"
			if part.Type == "file" {
				marker = "[Tool result PDF attached in the following user message.]"
				label = "PDF from tool result " + block.ToolUseID + ":"
			}
			texts = append(texts, marker)
			attachments = append(attachments, chatPart{Type: "text", Text: &label}, part)
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
	if len(name) == 0 || len(name) > maxToolNameBytes {
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
