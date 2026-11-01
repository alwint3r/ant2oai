package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type streamTool struct {
	id           string
	name         string
	arguments    strings.Builder
	contentIndex int
	isStarted    bool
}

type streamTranslator struct {
	declaredTools map[string]bool
	tools         map[int]*streamTool
	nextIndex     int
	textIndex     int
	activeTool    *streamTool
	trailingText  strings.Builder
	finishReason  string
	hasRefusal    bool
	usage         messageUsage
}

func newStreamTranslator(tools []inputTool) *streamTranslator {
	return &streamTranslator{
		declaredTools: declaredToolNames(tools), tools: make(map[int]*streamTool), textIndex: -1,
	}
}

// consume owns protocol state only. Its caller owns I/O, timeouts, and flushing.
// Text before tools and the first tool's arguments are forwarded immediately.
// Other parallel tools and later text wait for finish so downstream blocks stay
// sequential, as Anthropic SDK contentBlock callbacks require. Tool arguments
// are retained for validation before content_block_stop permits execution.
func (translator *streamTranslator) consume(chunk completionResponse) ([]messageEvent, error) {
	if hasValue(chunk.Error) {
		return nil, errors.New("upstream sent a streaming error")
	}
	if chunk.Usage != nil {
		usage, err := translateUsage(chunk.Usage)
		if err != nil {
			return nil, err
		}
		translator.usage = usage
	}

	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 || translator.finishReason != "" {
		return nil, errors.New("upstream sent unexpected streaming choices")
	}
	choice := chunk.Choices[0]
	if choice.Delta.Role != "" && choice.Delta.Role != "assistant" || hasValue(choice.Delta.FunctionCall) {
		return nil, errors.New("upstream sent an unsupported streaming delta")
	}

	var events []messageEvent
	if choice.Delta.Content != nil && *choice.Delta.Content != "" {
		if err := translator.addText(*choice.Delta.Content, &events); err != nil {
			return nil, err
		}
	}
	if choice.Delta.Refusal != nil && *choice.Delta.Refusal != "" {
		translator.hasRefusal = true
		if err := translator.addText(*choice.Delta.Refusal, &events); err != nil {
			return nil, err
		}
	}

	for _, delta := range choice.Delta.ToolCalls {
		if delta.Index == nil || *delta.Index < 0 || *delta.Index >= maxToolCalls {
			return nil, errors.New("upstream sent an invalid tool call index")
		}
		if delta.Type != "" && delta.Type != "function" {
			return nil, errors.New("upstream sent an unsupported tool call type")
		}
		tool, exists := translator.tools[*delta.Index]
		if !exists {
			tool = &streamTool{}
			translator.tools[*delta.Index] = tool
		}
		if tool.isStarted {
			if delta.ID != "" && delta.ID != tool.id || delta.Function.Name != "" && delta.Function.Name != tool.name {
				return nil, errors.New("upstream changed tool metadata after argument streaming began")
			}
		} else {
			tool.id += delta.ID
			tool.name += delta.Function.Name
			if len(tool.id) > 256 || len(tool.name) > 64 {
				return nil, errors.New("upstream tool metadata exceeds the supported limits")
			}
		}

		if tool.arguments.Len()+len(delta.Function.Arguments) > maxToolArguments {
			return nil, errors.New("upstream tool arguments exceed 8 MiB")
		}
		tool.arguments.WriteString(delta.Function.Arguments)
		if tool.isStarted {
			if delta.Function.Arguments != "" {
				events = append(events, argumentEvent(tool.contentIndex, delta.Function.Arguments))
			}
		} else if translator.activeTool == nil && tool.id != "" &&
			translator.declaredTools[tool.name] && tool.arguments.Len() > 0 {
			if err := translator.startTool(tool, &events); err != nil {
				return nil, err
			}
		}
	}

	if choice.FinishReason != nil {
		if *choice.FinishReason == "" {
			return nil, errors.New("upstream sent an empty finish reason")
		}
		translator.finishReason = *choice.FinishReason
	}
	return events, nil
}

func (translator *streamTranslator) addText(text string, events *[]messageEvent) error {
	if translator.activeTool != nil {
		if translator.trailingText.Len()+len(text) > maxResponseBytes {
			return errors.New("upstream text after tool calls exceeds 64 MiB")
		}
		translator.trailingText.WriteString(text)
		return nil
	}

	if translator.textIndex < 0 {
		index, err := translator.openBlock()
		if err != nil {
			return err
		}
		translator.textIndex = index
		empty := ""
		*events = append(*events, messageEvent{
			Type: "content_block_start", Index: &index, ContentBlock: &outputBlock{Type: "text", Text: &empty},
		})
	}
	index := translator.textIndex
	*events = append(*events, messageEvent{
		Type: "content_block_delta", Index: &index,
		Delta: map[string]string{"type": "text_delta", "text": text},
	})
	return nil
}

func (translator *streamTranslator) openBlock() (int, error) {
	if translator.nextIndex >= maxContentBlocks {
		return 0, errors.New("upstream exceeded the limit of 1024 content blocks")
	}
	index := translator.nextIndex
	translator.nextIndex++
	return index, nil
}

func (translator *streamTranslator) startTool(tool *streamTool, events *[]messageEvent) error {
	if translator.textIndex >= 0 {
		index := translator.textIndex
		*events = append(*events, messageEvent{Type: "content_block_stop", Index: &index})
		translator.textIndex = -1
	}
	index, err := translator.openBlock()
	if err != nil {
		return err
	}
	tool.contentIndex = index
	tool.isStarted = true
	translator.activeTool = tool
	*events = append(*events, messageEvent{
		Type: "content_block_start", Index: &index,
		ContentBlock: &outputBlock{Type: "tool_use", ID: tool.id, Name: tool.name, Input: json.RawMessage("{}")},
	})
	if tool.arguments.Len() > 0 {
		*events = append(*events, argumentEvent(index, tool.arguments.String()))
	}
	return nil
}

func argumentEvent(index int, arguments string) messageEvent {
	return messageEvent{
		Type: "content_block_delta", Index: &index,
		Delta: map[string]string{"type": "input_json_delta", "partial_json": arguments},
	}
}

func (translator *streamTranslator) finish() ([]messageEvent, error) {
	if translator.finishReason == "" {
		return nil, errors.New("upstream stream ended before a finish reason")
	}
	stopReason, err := translateStopReason(
		translator.finishReason, len(translator.tools) > 0, translator.hasRefusal,
	)
	if err != nil {
		return nil, err
	}

	seenIDs := make(map[string]bool)
	// Validate every tool before closing any tool block. An interrupted or
	// malformed argument object must not become an executable tool call.
	for _, tool := range translator.tools {
		if tool.id == "" || !translator.declaredTools[tool.name] || seenIDs[tool.id] {
			return nil, errors.New("upstream returned invalid or undeclared tool metadata")
		}
		if _, err := toolArguments(tool.arguments.String()); err != nil {
			return nil, err
		}
		seenIDs[tool.id] = true
	}

	var events []messageEvent
	if translator.activeTool != nil {
		index := translator.activeTool.contentIndex
		events = append(events, messageEvent{Type: "content_block_stop", Index: &index})
		translator.activeTool = nil
	}

	// Replay buffered parallel tools as complete, sequential content blocks.
	for upstreamIndex := 0; upstreamIndex < maxToolCalls; upstreamIndex++ {
		tool, exists := translator.tools[upstreamIndex]
		if exists && !tool.isStarted {
			if err := translator.startTool(tool, &events); err != nil {
				return nil, err
			}
			index := tool.contentIndex
			events = append(events, messageEvent{Type: "content_block_stop", Index: &index})
			translator.activeTool = nil
		}
	}
	if translator.trailingText.Len() > 0 {
		if err := translator.addText(translator.trailingText.String(), &events); err != nil {
			return nil, err
		}
	}
	if translator.nextIndex == 0 {
		if err := translator.addText("", &events); err != nil {
			return nil, err
		}
	}
	if translator.textIndex >= 0 {
		index := translator.textIndex
		events = append(events, messageEvent{Type: "content_block_stop", Index: &index})
		translator.textIndex = -1
	}

	events = append(events,
		messageEvent{
			Type: "message_delta", Delta: map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			Usage: &translator.usage,
		},
		messageEvent{Type: "message_stop"},
	)
	return events, nil
}

type sseFrame struct {
	name string
	data []byte
	err  error
}

// readSSE blocks on its reader and on a bounded channel. The caller must cancel
// ctx and close the response body on exit to release both kinds of wait.
func readSSE(ctx context.Context, body io.Reader, frames chan<- sseFrame) {
	defer close(frames)
	send := func(frame sseFrame) bool {
		select {
		case frames <- frame:
			return true
		case <-ctx.Done():
			return false
		}
	}

	limited := &io.LimitedReader{R: body, N: maxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxEventBytes+1)
	var data []byte
	name := ""
	isFirstLine := true
	for scanner.Scan() {
		if limited.N <= 0 {
			send(sseFrame{err: errors.New("upstream stream exceeds 64 MiB")})
			return
		}
		line := scanner.Bytes()
		if isFirstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			isFirstLine = false
		}

		if len(line) == 0 {
			if !send(sseFrame{name: name, data: data}) {
				return
			}
			data = nil
			name = ""
			continue
		}
		if line[0] == ':' {
			if !send(sseFrame{}) {
				return
			}
			continue
		}

		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			if len(data)+len(value)+1 > maxEventBytes {
				send(sseFrame{err: errors.New("upstream SSE event exceeds 2 MiB")})
				return
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}

	if scanner.Err() != nil || limited.N <= 0 {
		send(sseFrame{err: errors.New("upstream stream read failed or exceeded its size limit")})
		return
	}
	if len(data) > 0 {
		send(sseFrame{name: name, data: data})
	}
}
