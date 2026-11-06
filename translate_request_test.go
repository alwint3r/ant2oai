package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	toolsField = `"tools":[{"name":"get_weather","description":"Weather.",` +
		`"input_schema":{"type":"object"}}]`
	translatedTools = `"tools":[{"type":"function","function":{"name":"get_weather",` +
		`"description":"Weather.","parameters":{"type":"object"}}}]`
	userMessages = `"messages":[{"role":"user","content":"hi"}]`
	base64Hello  = "aGVsbG8="
)

// requestWith returns a valid request with extra top-level fields.
func requestWith(fields string) string {
	return `{"model":"claude","max_tokens":10,` + fields + `}`
}

// userContent returns a request with one user message that has the given blocks.
func userContent(blocks string) string {
	return requestWith(`"messages":[{"role":"user","content":[` + blocks + `]}]`)
}

func decodeRequest(t *testing.T, source string) messagesRequest {
	t.Helper()

	var request messagesRequest
	if err := json.Unmarshal([]byte(source), &request); err != nil {
		t.Fatalf("decode request fixture: %v", err)
	}
	return request
}

func TestTranslateRequest(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*config)
		input     string
		want      string
	}{
		{
			name:  "string content",
			input: simpleRequest,
			want:  `{"model":"claude","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name:  "text blocks become one string",
			input: userContent(`{"type":"text","text":"a"},{"type":"text","text":"b"}`),
			want: `{"model":"claude","max_tokens":10,"stream":false,
				"messages":[{"role":"user","content":"a\n\nb"}]}`,
		},
		{
			name:  "system string",
			input: requestWith(`"system":"be brief",` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "system blocks",
			input: requestWith(`"system":[{"type":"text","text":"one"},
				{"type":"text","text":"two"}],` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"system","content":"one\n\ntwo"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "system message in history",
			input: requestWith(`"messages":[{"role":"system","content":"note"},
				{"role":"user","content":"hi"}]`),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"system","content":"note"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "sampling and stop sequences",
			input: requestWith(`"temperature":0.5,"top_p":0.9,"stop_sequences":["a","b"],` +
				userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,"temperature":0.5,
				"top_p":0.9,"stop":["a","b"],` + userMessages + `}`,
		},
		{
			name:      "omit sampling",
			configure: func(cfg *config) { cfg.omitSampling = true },
			input:     requestWith(`"temperature":0.5,"top_p":0.9,` + userMessages),
			want:      `{"model":"claude","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name:      "max_completion_tokens field",
			configure: func(cfg *config) { cfg.tokenLimitField = "max_completion_tokens" },
			input:     simpleRequest,
			want: `{"model":"claude","max_completion_tokens":10,"stream":false,` +
				userMessages + `}`,
		},
		{
			name:  "stream asks for usage",
			input: streamingRequest,
			want: `{"model":"claude","max_tokens":10,"stream":true,
				"stream_options":{"include_usage":true},` + userMessages + `}`,
		},
		{
			name:      "stream usage disabled",
			configure: func(cfg *config) { cfg.streamUsage = false },
			input:     streamingRequest,
			want:      `{"model":"claude","max_tokens":10,"stream":true,` + userMessages + `}`,
		},
		{
			name: "model map has priority over the fallback model",
			configure: func(cfg *config) {
				cfg.modelMap = map[string]string{"claude": "mapped"}
				cfg.model = "fallback"
			},
			input: simpleRequest,
			want:  `{"model":"mapped","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name:      "fallback model",
			configure: func(cfg *config) { cfg.model = "fallback" },
			input:     simpleRequest,
			want:      `{"model":"fallback","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name:  "effort is forwarded",
			input: requestWith(`"output_config":{"effort":"max"},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,
				"reasoning_effort":"max",` + userMessages + `}`,
		},
		{
			name:      "omit effort",
			configure: func(cfg *config) { cfg.omitReasoningEffort = true },
			input:     requestWith(`"output_config":{"effort":"low"},` + userMessages),
			want:      `{"model":"claude","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name: "ignored fields",
			input: requestWith(`"metadata":{"user_id":"u"},"thinking":{"type":"enabled"},
				"cache_control":{"type":"ephemeral"},"service_tier":"auto",` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + userMessages + `}`,
		},
		{
			name:  "tools without a choice",
			input: requestWith(toolsField + `,` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,` + userMessages + `}`,
		},
		{
			name:  "tool choice auto",
			input: requestWith(toolsField + `,"tool_choice":{"type":"auto"},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,"tool_choice":"auto",` + userMessages + `}`,
		},
		{
			name:  "tool choice none",
			input: requestWith(toolsField + `,"tool_choice":{"type":"none"},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,"tool_choice":"none",` + userMessages + `}`,
		},
		{
			name:  "tool choice any",
			input: requestWith(toolsField + `,"tool_choice":{"type":"any"},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,"tool_choice":"required",` + userMessages + `}`,
		},
		{
			name: "named tool choice",
			input: requestWith(toolsField +
				`,"tool_choice":{"type":"tool","name":"get_weather"},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,"tool_choice":{"type":"function","function":{"name":"get_weather"}},` +
				userMessages + `}`,
		},
		{
			name: "parallel tool use disabled",
			input: requestWith(toolsField +
				`,"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` + userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools +
				`,"tool_choice":"auto","parallel_tool_calls":false,` + userMessages + `}`,
		},
		{
			name: "parallel setting needs tools",
			input: requestWith(`"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` +
				userMessages),
			want: `{"model":"claude","max_tokens":10,"stream":false,"tool_choice":"auto",` +
				userMessages + `}`,
		},
		{
			name: "tool history",
			input: requestWith(toolsField + `,"messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":[
					{"type":"thinking","thinking":"hmm","signature":"s"},
					{"type":"redacted_thinking","data":"x"},
					{"type":"text","text":"checking"},
					{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"a"}},
					{"type":"tool_use","id":"call_2","name":"get_weather","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call_1","content":"sunny"},
					{"type":"tool_result","tool_use_id":"call_2","is_error":true,
					 "content":[{"type":"text","text":"failed"}]},
					{"type":"text","text":"and tomorrow?"}]}]`),
			want: `{"model":"claude","max_tokens":10,"stream":false,` + translatedTools + `,
				"messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":"checking","tool_calls":[
					{"id":"call_1","type":"function",
					 "function":{"name":"get_weather","arguments":"{\"city\":\"a\"}"}},
					{"id":"call_2","type":"function",
					 "function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"sunny"},
				{"role":"tool","tool_call_id":"call_2","content":"Tool error: failed"},
				{"role":"user","content":"and tomorrow?"}]}`,
		},
		{
			name: "assistant with only tool calls has null content",
			input: requestWith(`"messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":[
					{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1"}]}]`),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"user","content":"go"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function",
					 "function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":""}]}`,
		},
		{
			name: "assistant with only thinking is dropped",
			input: requestWith(`"messages":[{"role":"user","content":"a"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]},
				{"role":"user","content":"b"}]`),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
		},
		{
			name: "tool attachments follow every tool reply",
			input: requestWith(`"messages":[{"role":"user","content":"look"},
				{"role":"assistant","content":[
					{"type":"tool_use","id":"call_1","name":"get_weather","input":{}},
					{"type":"tool_use","id":"call_2","name":"get_weather","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1",
					"content":[{"type":"text","text":"see"},
						{"type":"image","source":{"type":"base64","media_type":"image/png",
						 "data":"` + base64Hello + `"}},
						{"type":"document","source":{"type":"base64",
						 "media_type":"application/pdf","data":"` + base64Hello + `"}}]}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2"},
					{"type":"text","text":"done"}]}]`),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"user","content":"look"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function",
					 "function":{"name":"get_weather","arguments":"{}"}},
					{"id":"call_2","type":"function",
					 "function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":
					"see\n\n[Tool result image attached in the following user message.]\n\n[Tool result PDF attached in the following user message.]"},
				{"role":"tool","tool_call_id":"call_2","content":""},
				{"role":"user","content":[
					{"type":"text","text":"Image from tool result call_1:"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
				base64Hello + `"}},
					{"type":"text","text":"PDF from tool result call_1:"},
					{"type":"file","file":{"filename":"document.pdf",
					 "file_data":"data:application/pdf;base64,` + base64Hello + `"}},
					{"type":"text","text":"done"}]}]}`,
		},
		{
			name: "images and documents",
			input: userContent(`{"type":"text","text":"see"},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg",
				 "data":"` + base64Hello + `"}},
				{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf",
				 "data":"` + base64Hello + `"}}`),
			want: `{"model":"claude","max_tokens":10,"stream":false,"messages":[
				{"role":"user","content":[{"type":"text","text":"see"},
				{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` +
				base64Hello + `"}},
				{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},
				{"type":"file","file":{"filename":"document.pdf",
				 "file_data":"data:application/pdf;base64,` + base64Hello + `"}}]}]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testConfig()
			if testCase.configure != nil {
				testCase.configure(&cfg)
			}

			result, err := translateRequest(decodeRequest(t, testCase.input), cfg)
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}
			assertJSONEqual(t, marshal(t, result), testCase.want)
		})
	}
}

func TestTranslateRequestAcceptsSizeLimits(t *testing.T) {
	longModel := strings.Repeat("m", 256)
	longTool := strings.Repeat("t", 64)
	manyToolUses := toolUseBlocks(maxToolCalls)
	manyToolResults := toolResultBlocks(maxToolCalls)

	cases := []struct {
		name  string
		input string
	}{
		{"model of 256 bytes", `{"model":"` + longModel + `","max_tokens":1,` + userMessages + `}`},
		{"four stop sequences", requestWith(`"stop_sequences":["a","b","c","d"],` + userMessages)},
		{"temperature limits", requestWith(`"temperature":1,"top_p":0,` + userMessages)},
		{
			"tool name of 64 bytes",
			requestWith(`"tools":[{"name":"` + longTool + `","input_schema":{}}],` + userMessages),
		},
		{
			"128 tool calls in a message",
			requestWith(`"messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":[` + manyToolUses + `]},
				{"role":"user","content":[` + manyToolResults + `]}]`),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := translateRequest(decodeRequest(t, testCase.input), testConfig())
			if err != nil {
				t.Errorf("translateRequest: %v", err)
			}
		})
	}
}

func toolUseBlocks(count int) string {
	blocks := make([]string, 0, count)
	for index := 0; index < count; index++ {
		blocks = append(blocks, fmt.Sprintf(
			`{"type":"tool_use","id":"call_%d","name":"get_weather","input":{}}`, index))
	}
	return strings.Join(blocks, ",")
}

func toolResultBlocks(count int) string {
	blocks := make([]string, 0, count)
	for index := 0; index < count; index++ {
		blocks = append(blocks,
			fmt.Sprintf(`{"type":"tool_result","tool_use_id":"call_%d"}`, index))
	}
	return strings.Join(blocks, ",")
}

func TestTranslateRequestRejectsInvalidInput(t *testing.T) {
	const pendingToolUse = `{"role":"user","content":"go"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}]}`
	const twoPendingToolUses = `{"role":"user","content":"go"},
		{"role":"assistant","content":[
			{"type":"tool_use","id":"call_1","name":"get_weather","input":{}},
			{"type":"tool_use","id":"call_2","name":"get_weather","input":{}}]}`
	const toolUseInHistory = `{"role":"user","content":"go"},{"role":"assistant","content":[`
	const finalUser = `]},{"role":"user","content":"next"}`
	const base64Image = `"source":{"type":"base64","media_type":"image/png",` +
		`"data":"` + base64Hello + `"}`
	const base64PDF = `"source":{"type":"base64","media_type":"application/pdf",` +
		`"data":"` + base64Hello + `"}`
	const unsupportedBlock = "supported user blocks are text, images, PDF documents, " +
		"and tool results"
	const betasHint = "set CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"

	cases := []struct {
		name  string
		input string
		want  string
	}{
		// Required fields.
		{
			"empty model", `{"model":" ","max_tokens":1,` + userMessages + `}`,
			"model must be a non-empty ID of at most 256 bytes",
		},
		{
			"model over the limit",
			`{"model":"` + strings.Repeat("m", 257) + `","max_tokens":1,` + userMessages + `}`,
			"model must be a non-empty ID of at most 256 bytes",
		},
		{
			"zero max_tokens", `{"model":"m","max_tokens":0,` + userMessages + `}`,
			"max_tokens must be positive",
		},
		{
			"no messages", requestWith(`"messages":[]`),
			"messages must contain at least one message",
		},
		{
			"assistant prefill", requestWith(`"messages":[{"role":"assistant","content":"x"}]`),
			"assistant prefill is unsupported; end messages with user content or tool results",
		},

		// Sampling.
		{
			"negative temperature", requestWith(`"temperature":-0.1,` + userMessages),
			"temperature must be between 0 and 1",
		},
		{
			"high temperature", requestWith(`"temperature":1.1,` + userMessages),
			"temperature must be between 0 and 1",
		},
		{
			"top_p out of range", requestWith(`"top_p":1.1,` + userMessages),
			"top_p must be between 0 and 1",
		},
		{
			"top_k", requestWith(`"top_k":5,` + userMessages),
			"top_k has no Chat Completions equivalent",
		},
		{
			"five stop sequences",
			requestWith(`"stop_sequences":["a","b","c","d","e"],` + userMessages),
			"Chat Completions accepts at most four stop_sequences",
		},
		{
			"empty stop sequence", requestWith(`"stop_sequences":[""],` + userMessages),
			"stop_sequences must contain non-empty strings",
		},

		// Experimental options.
		{
			"context management", requestWith(`"context_management":{"a":1},` + userMessages),
			"context_management: Extra inputs are not permitted; " + betasHint,
		},
		{
			"output format", requestWith(`"output_config":{"format":{"a":1}},` + userMessages),
			"output_config format and task_budget are unsupported; " + betasHint,
		},
		{
			"task budget", requestWith(`"output_config":{"task_budget":1},` + userMessages),
			"output_config format and task_budget are unsupported; " + betasHint,
		},
		{
			"effort type", requestWith(`"output_config":{"effort":1},` + userMessages),
			"output_config.effort must be a string",
		},
		{
			"unknown effort", requestWith(`"output_config":{"effort":"tiny"},` + userMessages),
			"output_config.effort must be low, medium, high, xhigh, or max",
		},
		{
			"service tier", requestWith(`"service_tier":"priority",` + userMessages),
			"only service_tier auto is supported",
		},

		// Tools.
		{
			"deferred tool",
			requestWith(`"tools":[{"name":"a","input_schema":{},"defer_loading":true}],` +
				userMessages),
			"defer_loading requires Anthropic tool search; set ENABLE_TOOL_SEARCH=false",
		},
		{
			"server tool",
			requestWith(`"tools":[{"type":"web_search","name":"a","input_schema":{}}],` +
				userMessages),
			"server-managed tools are unsupported; " +
				"use client-side tools and set ENABLE_TOOL_SEARCH=false",
		},
		{
			"invalid tool name",
			requestWith(`"tools":[{"name":"bad name","input_schema":{}}],` + userMessages),
			"tools[0] requires a unique function name: " +
				"1-64 ASCII letters, digits, underscores, or dashes",
		},
		{
			"tool name over the limit",
			requestWith(`"tools":[{"name":"` + strings.Repeat("t", 65) +
				`","input_schema":{}}],` + userMessages),
			"tools[0] requires a unique function name: " +
				"1-64 ASCII letters, digits, underscores, or dashes",
		},
		{
			"duplicate tool",
			requestWith(`"tools":[{"name":"a","input_schema":{}},
				{"name":"a","input_schema":{}}],` + userMessages),
			"tools[1] requires a unique function name: " +
				"1-64 ASCII letters, digits, underscores, or dashes",
		},
		{
			"tool schema type",
			requestWith(`"tools":[{"name":"a","input_schema":[]}],` + userMessages),
			"tools[0].input_schema must be a JSON object",
		},
		{
			"tool choice any without tools",
			requestWith(`"tool_choice":{"type":"any"},` + userMessages),
			"tool_choice any requires tools",
		},
		{
			"tool choice for an undeclared tool",
			requestWith(`"tool_choice":{"type":"tool","name":"a"},` + userMessages),
			"tool_choice must name a declared tool",
		},
		{
			"unknown tool choice", requestWith(`"tool_choice":{"type":"some"},` + userMessages),
			"tool_choice type must be auto, none, any, or tool",
		},

		// System prompt.
		{
			"system type", requestWith(`"system":1,` + userMessages),
			"system: content must be a string or an array of content blocks",
		},
		{
			"non-text system block",
			requestWith(`"system":[{"type":"image"}],` + userMessages),
			"system must contain only text blocks",
		},

		// Message structure.
		{
			"message output config",
			requestWith(`"messages":[{"role":"user","content":"x",
				"output_config":{"format":{"a":1}}}]`),
			"messages[0].output_config format and task_budget are unsupported",
		},
		{
			"missing content", requestWith(`"messages":[{"role":"user"}]`),
			"messages[0]: content is required",
		},
		{
			"content type", requestWith(`"messages":[{"role":"user","content":1}]`),
			"messages[0]: content must be a string or an array of content blocks",
		},
		{
			"invalid block structure", requestWith(`"messages":[{"role":"user","content":[1]}]`),
			"messages[0]: invalid content block structure",
		},
		{
			"unknown role", requestWith(`"messages":[{"role":"tool","content":"x"}]`),
			"messages[0].role must be user, assistant, or system",
		},
		{
			"empty conversation", requestWith(`"messages":[{"role":"user","content":[]}]`),
			"messages must end with user content, system content, or tool results",
		},

		// Assistant blocks.
		{
			"assistant text block without text",
			requestWith(`"messages":[` + toolUseInHistory + `{"type":"text"}` + finalUser + `]`),
			"text blocks require a text string",
		},
		{
			"tool_use without an ID",
			requestWith(`"messages":[` + toolUseInHistory +
				`{"type":"tool_use","name":"a","input":{}}` + finalUser + `]`),
			"tool_use requires a unique ID of at most 256 bytes",
		},
		{
			"tool_use ID over the limit",
			requestWith(`"messages":[` + toolUseInHistory + `{"type":"tool_use","id":"` +
				strings.Repeat("i", 257) + `","name":"a","input":{}}` + finalUser + `]`),
			"tool_use requires a unique ID of at most 256 bytes",
		},
		{
			"repeated tool_use ID",
			requestWith(`"messages":[` + toolUseInHistory +
				`{"type":"tool_use","id":"a","name":"a","input":{}},
				 {"type":"tool_use","id":"a","name":"a","input":{}}` + finalUser + `]`),
			"tool_use requires a unique ID of at most 256 bytes",
		},
		{
			"tool_use input type",
			requestWith(`"messages":[` + toolUseInHistory +
				`{"type":"tool_use","id":"a","name":"a","input":[]}` + finalUser + `]`),
			"tool_use requires an OpenAI function name and a JSON object input",
		},
		{
			"tool_use name",
			requestWith(`"messages":[` + toolUseInHistory +
				`{"type":"tool_use","id":"a","name":"bad name","input":{}}` + finalUser + `]`),
			"tool_use requires an OpenAI function name and a JSON object input",
		},
		{
			"too many tool calls",
			requestWith(`"messages":[` + toolUseInHistory + toolUseBlocks(maxToolCalls+1) +
				finalUser + `]`),
			"a message exceeds the limit of 128 tool calls",
		},
		{
			"unknown assistant block",
			requestWith(`"messages":[` + toolUseInHistory + `{"type":"image"}` + finalUser + `]`),
			"unsupported assistant content block",
		},

		// Tool result order.
		{
			"assistant before a tool result",
			requestWith(`"messages":[` + pendingToolUse + `,
				{"role":"assistant","content":"x"},{"role":"user","content":"y"}]`),
			"every tool_use must receive a tool_result before the next assistant message",
		},
		{
			"tool result without a tool use",
			userContent(`{"type":"tool_result","tool_use_id":"call_1"}`),
			"tool_result must match an unanswered tool_use in a user message",
		},
		{
			"tool result in a system message",
			requestWith(`"messages":[` + pendingToolUse + `,{"role":"system","content":[
				{"type":"tool_result","tool_use_id":"call_1"}]}]`),
			"tool_result must match an unanswered tool_use in a user message",
		},
		{
			"tool result answered twice",
			requestWith(`"messages":[` + pendingToolUse + `,{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1"},
				{"type":"tool_result","tool_use_id":"call_1"}]}]`),
			"tool_result must match an unanswered tool_use in a user message",
		},
		{
			"user content before all tool results",
			requestWith(`"messages":[` + twoPendingToolUses + `,{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1"},
				{"type":"text","text":"early"}]}]`),
			"all parallel tool results must precede the next user or system content",
		},
		{
			"missing tool result at the end",
			requestWith(`"messages":[` + twoPendingToolUses + `,{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1"}]}]`),
			"messages end before all tool results are supplied",
		},
		{
			"invalid nested tool result block",
			requestWith(`"messages":[` + pendingToolUse + `,{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":[
					{"type":"text","text":"ok"},{"type":"mystery"}]}]}]`),
			"messages[2].tool_result: content[1]: unsupported content block; " + unsupportedBlock,
		},
		{
			"invalid tool result content",
			requestWith(`"messages":[` + pendingToolUse + `,{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":1}]}]`),
			"messages[2].tool_result: content must be a string or an array of content blocks",
		},

		// User blocks.
		{
			"text block without text", userContent(`{"type":"text"}`),
			"messages[0]: text blocks require a text string",
		},
		{
			"image without a source", userContent(`{"type":"image"}`),
			"messages[0]: images require a user message and a source",
		},
		{
			"image in a system message",
			requestWith(`"messages":[{"role":"system","content":[{"type":"image",` +
				base64Image + `}]},{"role":"user","content":"x"}]`),
			"messages[0]: images require a user message and a source",
		},
		{
			"image media type",
			userContent(`{"type":"image","source":{"type":"base64","media_type":"image/bmp",
				"data":"` + base64Hello + `"}}`),
			"messages[0]: image media_type must be image/jpeg, image/png, image/gif, " +
				"or image/webp",
		},
		{
			"image without data",
			userContent(`{"type":"image","source":{"type":"base64","media_type":"image/png"}}`),
			"messages[0]: base64 images require data",
		},
		{
			"invalid image base64",
			userContent(`{"type":"image","source":{"type":"base64","media_type":"image/png",
				"data":"not base64!"}}`),
			"messages[0]: image data must be valid base64",
		},
		{
			"image URL with credentials",
			userContent(`{"type":"image","source":{"type":"url","url":"https://a:b@c/d"}}`),
			"messages[0]: image URLs require an absolute URL without embedded credentials",
		},
		{
			"relative image URL",
			userContent(`{"type":"image","source":{"type":"url","url":"/a.png"}}`),
			"messages[0]: image URLs require an absolute URL without embedded credentials",
		},
		{
			"image URL scheme",
			userContent(`{"type":"image","source":{"type":"url","url":"ftp://a/b.png"}}`),
			"messages[0]: image URLs must use HTTP or HTTPS",
		},
		{
			"image source type", userContent(`{"type":"image","source":{"type":"file"}}`),
			"messages[0]: image source type must be base64 or url",
		},
		{
			"document without a source", userContent(`{"type":"document"}`),
			"messages[0]: PDF documents require a user message and a source",
		},
		{
			"document title", userContent(`{"type":"document","title":"t",` + base64PDF + `}`),
			"messages[0]: PDF document title, context, and citations are unsupported",
		},
		{
			"document context", userContent(`{"type":"document","context":"c",` + base64PDF + `}`),
			"messages[0]: PDF document title, context, and citations are unsupported",
		},
		{
			"document citations",
			userContent(`{"type":"document","citations":{"enabled":true},` + base64PDF + `}`),
			"messages[0]: PDF document title, context, and citations are unsupported",
		},
		{
			"document media type", userContent(`{"type":"document",` + base64Image + `}`),
			"messages[0]: document source must be base64 with media_type application/pdf",
		},
		{
			"document without data",
			userContent(`{"type":"document","source":{"type":"base64",
				"media_type":"application/pdf"}}`),
			"messages[0]: base64 PDF documents require data",
		},
		{
			"invalid document base64",
			userContent(`{"type":"document","source":{"type":"base64",
				"media_type":"application/pdf","data":"not base64!"}}`),
			"messages[0]: PDF document data must be non-empty valid base64",
		},
		{
			// The decoder skips line breaks, so this valid input decodes to nothing.
			"document that decodes to nothing",
			userContent(`{"type":"document","source":{"type":"base64",
				"media_type":"application/pdf","data":"\n"}}`),
			"messages[0]: PDF document data must be non-empty valid base64",
		},
		{
			"tool reference", userContent(`{"type":"tool_reference"}`),
			"messages[0]: tool_reference requires Anthropic tool search; " +
				"set ENABLE_TOOL_SEARCH=false and start a new session",
		},
		{
			"known unsupported block", userContent(`{"type":"audio"}`),
			"messages[0]: unsupported content block type audio; " + unsupportedBlock,
		},
		{
			"unknown type stays out of the message", userContent(`{"type":"secret-value"}`),
			"messages[0]: unsupported content block; " + unsupportedBlock,
		},

		// The first failed stage decides the error that the client sees.
		{
			"max_tokens is checked before sampling",
			`{"model":"m","max_tokens":0,"temperature":2,` + userMessages + `}`,
			"max_tokens must be positive",
		},
		{
			"sampling is checked before tools",
			requestWith(`"temperature":2,"tools":[{"name":"bad name"}],` + userMessages),
			"temperature must be between 0 and 1",
		},
		{
			"tools are checked before the system prompt",
			requestWith(`"tools":[{"name":"bad name"}],"system":1,` + userMessages),
			"tools[0] requires a unique function name: " +
				"1-64 ASCII letters, digits, underscores, or dashes",
		},
		{
			"the system prompt is checked before messages",
			requestWith(`"system":1,"messages":[{"role":"mystery","content":"x"}]`),
			"system: content must be a string or an array of content blocks",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := translateRequest(decodeRequest(t, testCase.input), testConfig())
			assertError(t, err, testCase.want)
		})
	}
}
