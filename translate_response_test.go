package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	declaredToolRequest = `{"model":"claude","max_tokens":10,` + toolsField + `,` +
		userMessages + `}`
	testUsage = `{"prompt_tokens":10,"completion_tokens":5,
		"prompt_tokens_details":{"cached_tokens":3}}`
	translatedTestUsage = `{"input_tokens":7,"output_tokens":5,
		"cache_creation_input_tokens":0,"cache_read_input_tokens":3}`
	emptyUsage = `{"input_tokens":0,"output_tokens":0,
		"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
)

func decodeCompletion(t *testing.T, source string) completionResponse {
	t.Helper()

	var response completionResponse
	if err := json.Unmarshal([]byte(source), &response); err != nil {
		t.Fatalf("decode completion fixture: %v", err)
	}
	return response
}

func completionWith(message, finishReason string) string {
	return `{"choices":[{"index":0,"finish_reason":"` + finishReason + `",
		"message":{"role":"assistant",` + message + `}}]}`
}

func TestTranslateResponse(t *testing.T) {
	const weatherCall = `"tool_calls":[{"id":"call_1","type":"function",
		"function":{"name":"get_weather","arguments":"{\"city\":\"a\"}"}}]`
	const weatherBlock = `{"type":"tool_use","id":"call_1","name":"get_weather",
		"input":{"city":"a"}}`

	cases := []struct {
		name       string
		completion string
		wantBlocks string
		wantReason string
		wantUsage  string
	}{
		{
			name:       "text",
			completion: simpleCompletion,
			wantBlocks: `[{"type":"text","text":"hello"}]`,
			wantReason: "end_turn",
		},
		{
			name: "usage excludes cached tokens from input",
			completion: `{"usage":` + testUsage + `,"choices":[{"index":0,
				"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`,
			wantBlocks: `[{"type":"text","text":"x"}]`,
			wantReason: "end_turn",
			wantUsage:  translatedTestUsage,
		},
		{
			name:       "empty reply has one empty text block",
			completion: completionWith(`"content":null`, "stop"),
			wantBlocks: `[{"type":"text","text":""}]`,
			wantReason: "end_turn",
		},
		{
			name:       "length",
			completion: completionWith(`"content":"x"`, "length"),
			wantBlocks: `[{"type":"text","text":"x"}]`,
			wantReason: "max_tokens",
		},
		{
			name:       "content filter",
			completion: completionWith(`"content":"x"`, "content_filter"),
			wantBlocks: `[{"type":"text","text":"x"}]`,
			wantReason: "refusal",
		},
		{
			name:       "refusal",
			completion: completionWith(`"refusal":"no"`, "stop"),
			wantBlocks: `[{"type":"text","text":"no"}]`,
			wantReason: "refusal",
		},
		{
			name:       "text before tool calls",
			completion: completionWith(`"content":"calling",`+weatherCall, "tool_calls"),
			wantBlocks: `[{"type":"text","text":"calling"},` + weatherBlock + `]`,
			wantReason: "tool_use",
		},
		{
			name:       "stop reason with tool calls",
			completion: completionWith(weatherCall, "stop"),
			wantBlocks: `[` + weatherBlock + `]`,
			wantReason: "tool_use",
		},
		{
			name: "empty tool arguments become an empty object",
			completion: completionWith(`"tool_calls":[{"id":"call_1","type":"function",
				"function":{"name":"get_weather","arguments":""}}]`, "tool_calls"),
			wantBlocks: `[{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}]`,
			wantReason: "tool_use",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := decodeRequest(t, declaredToolRequest)
			response := decodeCompletion(t, testCase.completion)

			result, err := translateResponse(response, request, "msg_1")
			if err != nil {
				t.Fatalf("translateResponse: %v", err)
			}

			wantUsage := testCase.wantUsage
			if wantUsage == "" {
				wantUsage = emptyUsage
			}
			want := `{"id":"msg_1","type":"message","role":"assistant","model":"claude",
				"content":` + testCase.wantBlocks + `,"stop_reason":"` + testCase.wantReason + `",
				"stop_sequence":null,"usage":` + wantUsage + `}`
			assertJSONEqual(t, marshal(t, result), want)
		})
	}
}

func TestTranslateResponseRejectsInvalidCompletion(t *testing.T) {
	toolCall := func(id, kind, name, arguments string) string {
		return fmt.Sprintf(`{"id":%q,"type":%q,"function":{"name":%q,"arguments":%q}}`,
			id, kind, name, arguments)
	}
	const usageError = "upstream returned invalid token usage"
	const invalidArguments = "upstream tool arguments must be a complete JSON object " +
		"within the nesting limit"

	cases := []struct {
		name       string
		completion string
		want       string
	}{
		{"no choices", `{"choices":[]}`, "upstream must return exactly one completion choice"},
		{
			"two choices",
			`{"choices":[{"index":0,"finish_reason":"stop"},{"index":1,"finish_reason":"stop"}]}`,
			"upstream must return exactly one completion choice",
		},
		{
			"choice index",
			`{"choices":[{"index":1,"finish_reason":"stop"}]}`,
			"upstream must return exactly one completion choice",
		},
		{
			"error field",
			`{"error":{"code":"x"},"choices":[{"index":0,"finish_reason":"stop"}]}`,
			"upstream must return exactly one completion choice",
		},
		{
			"no finish reason",
			`{"choices":[{"index":0,"message":{"role":"assistant","content":"x"}}]}`,
			"upstream response lacks a finish reason",
		},
		{
			"role",
			`{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"user"}}]}`,
			"upstream returned an unsupported assistant message",
		},
		{
			"legacy function call",
			completionWith(`"function_call":{"name":"a"}`, "stop"),
			"upstream returned an unsupported assistant message",
		},
		{
			"unknown finish reason",
			completionWith(`"content":"x"`, "other"),
			"upstream returned an unsupported finish reason",
		},
		{
			"tool_calls reason without tool calls",
			completionWith(`"content":"x"`, "tool_calls"),
			"upstream finished with tool_calls without any tool calls",
		},
		{
			"tool type",
			completionWith(`"tool_calls":[`+toolCall("a", "other", "get_weather", "{}")+`]`,
				"tool_calls"),
			"upstream returned invalid or duplicate tool metadata",
		},
		{
			"tool without an ID",
			completionWith(`"tool_calls":[`+toolCall("", "function", "get_weather", "{}")+`]`,
				"tool_calls"),
			"upstream returned invalid or duplicate tool metadata",
		},
		{
			"tool ID over the limit",
			completionWith(`"tool_calls":[`+
				toolCall(strings.Repeat("i", 257), "function", "get_weather", "{}")+`]`,
				"tool_calls"),
			"upstream returned invalid or duplicate tool metadata",
		},
		{
			"duplicate tool ID",
			completionWith(`"tool_calls":[`+toolCall("a", "function", "get_weather", "{}")+`,`+
				toolCall("a", "function", "get_weather", "{}")+`]`, "tool_calls"),
			"upstream returned invalid or duplicate tool metadata",
		},
		{
			"undeclared tool",
			completionWith(`"tool_calls":[`+toolCall("a", "function", "other", "{}")+`]`,
				"tool_calls"),
			"upstream returned invalid or undeclared tool metadata",
		},
		{
			"incomplete arguments",
			completionWith(`"tool_calls":[`+toolCall("a", "function", "get_weather", `{"a`)+`]`,
				"tool_calls"),
			invalidArguments,
		},
		{
			"array arguments",
			completionWith(`"tool_calls":[`+toolCall("a", "function", "get_weather", `[]`)+`]`,
				"tool_calls"),
			invalidArguments,
		},
		{
			"deep arguments",
			completionWith(`"tool_calls":[`+toolCall("a", "function", "get_weather",
				`{"a":`+strings.Repeat("[", 128)+strings.Repeat("]", 128)+`}`)+`]`, "tool_calls"),
			invalidArguments,
		},
		{
			"negative usage",
			`{"usage":{"prompt_tokens":-1},"choices":[{"index":0,"finish_reason":"stop",
				"message":{"role":"assistant","content":"x"}}]}`,
			usageError,
		},
		{
			"cached tokens exceed the prompt",
			`{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2}},
				"choices":[{"index":0,"finish_reason":"stop",
				"message":{"role":"assistant","content":"x"}}]}`,
			usageError,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := decodeRequest(t, declaredToolRequest)
			response := decodeCompletion(t, testCase.completion)

			_, err := translateResponse(response, request, "msg_1")
			assertError(t, err, testCase.want)
		})
	}
}

func TestTranslateResponseToolLimits(t *testing.T) {
	request := decodeRequest(t, declaredToolRequest)
	completion := func(callCount int, arguments string) completionResponse {
		reason := "tool_calls"
		calls := make([]toolCall, 0, callCount)
		for index := 0; index < callCount; index++ {
			calls = append(calls, toolCall{
				ID: fmt.Sprintf("call_%d", index), Type: "function",
				Function: functionCall{Name: "get_weather", Arguments: arguments},
			})
		}
		return completionResponse{Choices: []completionChoice{{
			Message:      completionMessage{Role: "assistant", ToolCalls: calls},
			FinishReason: &reason,
		}}}
	}

	if _, err := translateResponse(completion(maxToolCalls, "{}"), request, "m"); err != nil {
		t.Errorf("128 tool calls: %v", err)
	}
	_, err := translateResponse(completion(maxToolCalls+1, "{}"), request, "m")
	assertError(t, err, "upstream exceeded the limit of 128 tool calls")

	// The object has exactly the maximum size, because padding fills the rest.
	wrapper := `{"a":""}`
	largest := `{"a":"` + strings.Repeat("x", maxToolArguments-len(wrapper)) + `"}`
	if _, err := translateResponse(completion(1, largest), request, "m"); err != nil {
		t.Errorf("arguments of 8 MiB: %v", err)
	}
	oversized := `{"a":"` + strings.Repeat("x", maxToolArguments-len(wrapper)+1) + `"}`
	_, err = translateResponse(completion(1, oversized), request, "m")
	assertError(t, err, "upstream tool arguments exceed 8 MiB")
}
