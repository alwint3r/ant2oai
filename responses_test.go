package main

import (
	"fmt"
	"strings"
	"testing"
)

const (
	responseCreated = `{"type":"response.created","response":{"id":"resp_1"}}`
	messageItem     = `{"type":"message","id":"item_1","role":"assistant",
		"content":[{"type":"output_text","text":"Hello world"}]}`
	refusalItem = `{"type":"message","id":"item_1","role":"assistant",
		"content":[{"type":"refusal","refusal":"no"}]}`
	toolItem = `{"type":"function_call","id":"item_2","call_id":"call_a",
		"name":"get_weather","arguments":"{\"city\":\"x\"}"}`
	responsesUsage = `"usage":{"input_tokens":10,"output_tokens":5,
		"input_tokens_details":{"cached_tokens":3}}`
)

func itemAdded(index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","output_index":%d,"item":%s}`,
		index, item)
}

func itemDone(index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`,
		index, item)
}

func messageAdded(index int) string {
	return itemAdded(index, `{"type":"message","id":"item_1","role":"assistant"}`)
}

func toolAdded(index int) string {
	return itemAdded(index, `{"type":"function_call","id":"item_2","call_id":"call_a",
		"name":"get_weather"}`)
}

func outputTextDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","output_index":%d,
		"content_index":0,"item_id":"item_1","delta":%q}`, index, text)
}

func argumentsDelta(index int, arguments string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":%d,
		"item_id":"item_2","delta":%q}`, index, arguments)
}

func argumentsDone(index int, arguments string) string {
	return fmt.Sprintf(`{"type":"response.function_call_arguments.done","output_index":%d,
		"item_id":"item_2","arguments":%q}`, index, arguments)
}

func responseCompleted(output string) string {
	return `{"type":"response.completed","response":{"id":"resp_1","status":"completed",
		"output":` + output + `}}`
}

func responseIncomplete(reason, output string) string {
	return `{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete",
		"incomplete_details":{"reason":"` + reason + `"},"output":` + output + `}}`
}

// textResponse is a complete Responses stream with one text message.
func textResponse() []string {
	return []string{
		responseCreated, messageAdded(0), outputTextDelta(0, "Hello "), outputTextDelta(0, "world"),
		itemDone(0, messageItem),
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed",
			"output":[` + messageItem + `],` + responsesUsage + `}}`,
	}
}

// runResponses feeds events in order and then finishes the stream. It stops at
// the first error, as the HTTP layer does.
func runResponses(events []string) ([]messageEvent, *responsesStream, error) {
	translator := newResponsesStream([]inputTool{{Name: "get_weather"}})
	var output []messageEvent
	for _, event := range events {
		additional, err := translator.consume([]byte(event), "")
		if err != nil {
			return nil, translator, err
		}
		output = append(output, additional...)
	}

	final, err := translator.finish()
	if err != nil {
		return nil, translator, err
	}
	return append(output, final...), translator, nil
}

func TestTranslateResponsesRequest(t *testing.T) {
	const responsesTool = `{"type":"additional_tools","role":"developer","tools":[
		{"type":"function","name":"get_weather","description":"Weather.",
		 "parameters":{"type":"object"},"strict":false}]}`
	const userItem = `{"type":"message","role":"user","content":"hi"}`

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "text",
			input: simpleRequest,
			want:  `{"model":"claude","store":false,"stream":true,"input":[` + userItem + `]}`,
		},
		{
			name:  "system message becomes a developer message",
			input: requestWith(`"system":"be brief",` + userMessages),
			want: `{"model":"claude","store":false,"stream":true,"input":[
				{"type":"message","role":"developer","content":"be brief"},` + userItem + `]}`,
		},
		{
			name:  "effort",
			input: requestWith(`"output_config":{"effort":"high"},` + userMessages),
			want: `{"model":"claude","store":false,"stream":true,
				"reasoning":{"effort":"high"},"input":[` + userItem + `]}`,
		},
		{
			name:  "tools default to non-strict",
			input: requestWith(toolsField + `,` + userMessages),
			want: `{"model":"claude","store":false,"stream":true,"input":[` +
				responsesTool + `,` + userItem + `]}`,
		},
		{
			name: "strict setting is kept",
			input: requestWith(`"tools":[{"name":"a","input_schema":{},"strict":true}],` +
				userMessages),
			want: `{"model":"claude","store":false,"stream":true,"input":[
				{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"a","parameters":{},"strict":true}]},` + userItem + `]}`,
		},
		{
			name: "named tool choice is flat",
			input: requestWith(toolsField +
				`,"tool_choice":{"type":"tool","name":"get_weather",
				"disable_parallel_tool_use":true},` + userMessages),
			want: `{"model":"claude","store":false,"stream":true,
				"tool_choice":{"type":"function","name":"get_weather"},
				"parallel_tool_calls":false,"input":[` + responsesTool + `,` + userItem + `]}`,
		},
		{
			name:  "string tool choice",
			input: requestWith(toolsField + `,"tool_choice":{"type":"any"},` + userMessages),
			want: `{"model":"claude","store":false,"stream":true,"tool_choice":"required",
				"input":[` + responsesTool + `,` + userItem + `]}`,
		},
		{
			name: "tool history",
			input: requestWith(`"messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":[{"type":"text","text":"calling"},
				 {"type":"tool_use","id":"call_1","name":"get_weather","input":{"a":1}}]},
				{"role":"user","content":[
				 {"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]}]`),
			want: `{"model":"claude","store":false,"stream":true,"input":[
				{"type":"message","role":"user","content":"go"},
				{"type":"message","role":"assistant","content":"calling"},
				{"type":"function_call","call_id":"call_1","name":"get_weather",
				 "arguments":"{\"a\":1}"},
				{"type":"function_call_output","call_id":"call_1","output":"sunny"}]}`,
		},
		{
			name: "empty tool output is kept",
			input: requestWith(`"messages":[{"role":"user","content":"go"},
				{"role":"assistant","content":[
				 {"type":"tool_use","id":"call_1","name":"get_weather","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1"}]}]`),
			want: `{"model":"claude","store":false,"stream":true,"input":[
				{"type":"message","role":"user","content":"go"},
				{"type":"function_call","call_id":"call_1","name":"get_weather",
				 "arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":""}]}`,
		},
		{
			name: "multipart content",
			input: userContent(`{"type":"text","text":"see"},
				{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf",
				 "data":"` + base64Hello + `"}}`),
			want: `{"model":"claude","store":false,"stream":true,"input":[
				{"type":"message","role":"user","content":[
				 {"type":"input_text","text":"see"},
				 {"type":"input_image","image_url":"https://example.com/a.png","detail":"auto"},
				 {"type":"input_file","filename":"document.pdf",
				  "file_data":"data:application/pdf;base64,` + base64Hello + `"}]}]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			completion, err := translateRequest(decodeRequest(t, testCase.input), testConfig())
			if err != nil {
				t.Fatalf("translateRequest: %v", err)
			}

			result, err := translateResponsesRequest(completion)
			if err != nil {
				t.Fatalf("translateResponsesRequest: %v", err)
			}
			assertJSONEqual(t, marshal(t, result), testCase.want)
		})
	}
}

func TestTranslateResponsesRequestRejectsStreamingStops(t *testing.T) {
	input := requestWith(`"stream":true,"stop_sequences":["x"],` + userMessages)
	completion, err := translateRequest(decodeRequest(t, input), testConfig())
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}

	_, err = translateResponsesRequest(completion)
	assertError(t, err, "stop_sequences require stream: false in ChatGPT OAuth mode; "+
		"streaming stop sequences are unsupported")
}

func TestResponsesStream(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		want   []string
	}{
		{
			name:   "text with usage",
			events: textResponse(),
			want: append([]string{textStart(0), textDelta(0, "Hello "), textDelta(0, "world"),
				blockStop(0)}, messageEnd("end_turn", translatedTestUsage)...),
		},
		{
			name: "text only in the final item",
			events: []string{
				responseCreated, messageAdded(0), itemDone(0, messageItem),
				responseCompleted(`[` + messageItem + `]`),
			},
			want: append([]string{textStart(0), textDelta(0, "Hello world"), blockStop(0)},
				messageEnd("end_turn", emptyUsage)...),
		},
		{
			name: "refusal",
			events: []string{
				responseCreated, messageAdded(0),
				`{"type":"response.refusal.delta","output_index":0,"content_index":0,
					"item_id":"item_1","delta":"no"}`,
				itemDone(0, refusalItem), responseCompleted(`[` + refusalItem + `]`),
			},
			want: append([]string{textStart(0), textDelta(0, "no"), blockStop(0)},
				messageEnd("refusal", emptyUsage)...),
		},
		{
			name: "streamed tool arguments",
			events: []string{
				responseCreated, toolAdded(0), argumentsDelta(0, `{"city"`),
				argumentsDelta(0, `:"x"}`), argumentsDone(0, `{"city":"x"}`), itemDone(0, toolItem),
				responseCompleted(`[` + toolItem + `]`),
			},
			want: append([]string{
				toolStart(0, "call_a", "get_weather"),
				argumentDelta(0, `{"city"`), argumentDelta(0, `:"x"}`), blockStop(0),
			}, messageEnd("tool_use", emptyUsage)...),
		},
		{
			name: "tool arguments only in the done event",
			events: []string{
				responseCreated, toolAdded(0), argumentsDone(0, `{"city":"x"}`),
				itemDone(0, toolItem), responseCompleted(`[` + toolItem + `]`),
			},
			want: append([]string{
				toolStart(0, "call_a", "get_weather"), argumentDelta(0, `{"city":"x"}`),
				blockStop(0),
			}, messageEnd("tool_use", emptyUsage)...),
		},
		{
			name: "empty final output uses the verified item snapshots",
			events: []string{
				responseCreated, messageAdded(0), outputTextDelta(0, "Hello world"),
				itemDone(0, messageItem), responseCompleted(`[]`),
			},
			want: append([]string{textStart(0), textDelta(0, "Hello world"), blockStop(0)},
				messageEnd("end_turn", emptyUsage)...),
		},
		{
			name: "reasoning has no downstream block",
			events: []string{
				responseCreated, itemAdded(0, `{"type":"reasoning","id":"item_0"}`),
				itemDone(0, `{"type":"reasoning","id":"item_0"}`),
				messageAdded(1), outputTextDelta(1, "Hello world"), itemDone(1, messageItem),
				responseCompleted(`[{"type":"reasoning","id":"item_0"},` + messageItem + `]`),
			},
			want: append([]string{textStart(0), textDelta(0, "Hello world"), blockStop(0)},
				messageEnd("end_turn", emptyUsage)...),
		},
		{
			name: "max output tokens",
			events: []string{
				responseCreated, messageAdded(0), outputTextDelta(0, "Hello world"),
				responseIncomplete("max_output_tokens", `[`+messageItem+`]`),
			},
			want: append([]string{textStart(0), textDelta(0, "Hello world"), blockStop(0)},
				messageEnd("max_tokens", emptyUsage)...),
		},
		{
			name:   "content filter",
			events: []string{responseCreated, responseIncomplete("content_filter", `[]`)},
			want: append([]string{textStart(0), textDelta(0, ""), blockStop(0)},
				messageEnd("refusal", emptyUsage)...),
		},
		{
			name: "lifecycle events are ignored",
			events: []string{
				responseCreated, `{"type":"response.in_progress"}`, messageAdded(0),
				`{"type":"response.content_part.added","output_index":0,"content_index":0}`,
				outputTextDelta(0, "Hello world"), itemDone(0, messageItem),
				responseCompleted(`[` + messageItem + `]`),
			},
			want: append([]string{textStart(0), textDelta(0, "Hello world"), blockStop(0)},
				messageEnd("end_turn", emptyUsage)...),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events, translator, err := runResponses(testCase.events)
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			assertEvents(t, events, testCase.want)
			if translator.final == nil {
				t.Error("the stream ended without a final completion")
			}
		})
	}
}

func TestResponsesStreamRejectsInvalidEvents(t *testing.T) {
	const quotaMessage = "ChatGPT app or plan usage limit reached " +
		"(subscription_sharing_usage_limit_exceeded). " +
		"Review https://chatgpt.com/settings/usage before sending more requests."
	const genericFailure = "ChatGPT request failed. Check account permissions, model access, " +
		"OpenAI availability, and the upstream status and request-ID headers."
	const contextMessage = "context_length_exceeded: Prompt is too long: upstream context " +
		"window exceeded. Compact the conversation or select a larger-context model."
	const invalidOutput = "upstream returned invalid Responses output metadata"
	const unsupportedTool = "upstream returned an unsupported or undeclared Responses tool"
	const invalidText = "upstream returned invalid Responses text metadata"
	const unknownArguments = "upstream returned arguments for an unknown Responses tool"
	const unchangedArguments = "upstream changed Responses tool arguments after streaming them"
	const changedText = "upstream changed Responses text after streaming it"

	addedTool := func(fields string) string {
		return itemAdded(0, `{"type":"function_call","id":"item_2",`+fields+`}`)
	}
	otherText := strings.Replace(messageItem, "Hello world", "Other text", 1)
	longID := strings.Repeat("i", 257)

	cases := []struct {
		name   string
		events []string
		want   string
	}{
		// Event envelope.
		{"not JSON", []string{`{`}, "upstream sent an invalid Responses stream event"},
		{"no type", []string{`{}`}, "upstream sent an invalid Responses stream event"},
		{
			"too deep", []string{strings.Repeat("[", 129) + strings.Repeat("]", 129)},
			"upstream sent an invalid Responses stream event",
		},

		// Failure events carry only known codes.
		{
			"known error code",
			[]string{`{"type":"error","code":"subscription_sharing_usage_limit_exceeded"}`},
			quotaMessage,
		},
		{"unknown error code", []string{`{"type":"error","code":"other"}`}, genericFailure},
		{
			"failed response",
			[]string{`{"type":"response.failed",
				"response":{"error":{"code":"context_length_exceeded"}}}`},
			contextMessage,
		},
		{
			"completed response with an error",
			[]string{responseCreated, `{"type":"response.completed","response":{"id":"resp_1",
				"error":{"code":"other"}}}`},
			genericFailure,
		},

		// Creation.
		{
			"created without a response", []string{`{"type":"response.created"}`},
			"upstream returned invalid Responses creation metadata",
		},
		{
			"created without an ID", []string{`{"type":"response.created","response":{}}`},
			"upstream returned invalid Responses creation metadata",
		},
		{
			"created ID over the limit",
			[]string{`{"type":"response.created","response":{"id":"` + longID + `"}}`},
			"upstream returned invalid Responses creation metadata",
		},
		{
			"created twice", []string{responseCreated, responseCreated},
			"upstream returned invalid Responses creation metadata",
		},

		// New output items.
		{
			"item without an index", []string{`{"type":"response.output_item.added",
				"item":{"type":"message","id":"a","role":"assistant"}}`},
			invalidOutput,
		},
		{"item index out of order", []string{messageAdded(1)}, invalidOutput},
		{"item without data", []string{`{"type":"response.output_item.added",
			"output_index":0}`}, invalidOutput},
		{"item without an ID", []string{itemAdded(0, `{"type":"reasoning"}`)}, invalidOutput},
		{
			"item ID over the limit",
			[]string{itemAdded(0, `{"type":"reasoning","id":"`+longID+`"}`)}, invalidOutput,
		},
		{"repeated item ID", []string{messageAdded(0), messageAdded(1)}, invalidOutput},
		{
			"unknown item type", []string{itemAdded(0, `{"type":"other","id":"a"}`)},
			"upstream returned an unsupported Responses output item",
		},
		{
			"message role", []string{itemAdded(0, `{"type":"message","id":"a","role":"user"}`)},
			"upstream returned an unsupported Responses message role",
		},
		{
			"tool without a call ID", []string{addedTool(`"name":"get_weather"`)},
			unsupportedTool,
		},
		{
			"call ID over the limit",
			[]string{addedTool(`"call_id":"` + longID + `","name":"get_weather"`)},
			unsupportedTool,
		},
		{
			"undeclared tool", []string{addedTool(`"call_id":"a","name":"other"`)},
			unsupportedTool,
		},
		{
			"tool namespace",
			[]string{addedTool(`"call_id":"a","name":"get_weather","namespace":"x"`)},
			unsupportedTool,
		},

		// Text.
		{"text for an unknown item", []string{outputTextDelta(0, "x")}, invalidText},
		{"text for a tool", []string{toolAdded(0), outputTextDelta(0, "x")}, invalidText},
		{
			"text without a delta",
			[]string{messageAdded(0), `{"type":"response.output_text.delta","output_index":0,
				"content_index":0,"item_id":"item_1"}`},
			invalidText,
		},
		{
			"text without a content index",
			[]string{messageAdded(0), `{"type":"response.output_text.delta","output_index":0,
				"item_id":"item_1","delta":"x"}`},
			invalidText,
		},
		{
			"content index over the limit",
			[]string{messageAdded(0), `{"type":"response.output_text.delta","output_index":0,
				"content_index":1024,"item_id":"item_1","delta":"x"}`},
			invalidText,
		},
		{
			"text after refusal in the same part",
			[]string{
				messageAdded(0), outputTextDelta(0, "x"),
				`{"type":"response.refusal.delta","output_index":0,"content_index":0,
					"item_id":"item_1","delta":"y"}`,
			},
			"upstream changed the type of a Responses content part",
		},

		// Tool arguments.
		{"arguments for an unknown item", []string{argumentsDelta(0, "{}")}, unknownArguments},
		{
			"arguments for a message",
			[]string{messageAdded(0), `{"type":"response.function_call_arguments.delta",
				"output_index":0,"item_id":"item_1","delta":"{}"}`},
			unknownArguments,
		},
		{
			"arguments delta without a delta",
			[]string{toolAdded(0), `{"type":"response.function_call_arguments.delta",
				"output_index":0,"item_id":"item_2"}`},
			"upstream returned an invalid Responses argument event",
		},
		{
			"arguments delta after done",
			[]string{toolAdded(0), argumentsDone(0, "{}"), argumentsDelta(0, "{}")},
			"upstream returned an invalid Responses argument event",
		},
		{
			"arguments done without arguments",
			[]string{toolAdded(0), `{"type":"response.function_call_arguments.done",
				"output_index":0,"item_id":"item_2"}`},
			"upstream returned an invalid Responses argument event",
		},
		{
			"arguments change in the done event",
			[]string{toolAdded(0), argumentsDelta(0, "{}"), argumentsDone(0, `{"a":1}`)},
			unchangedArguments,
		},
		{
			"arguments with the same size change in the done event",
			[]string{toolAdded(0), argumentsDelta(0, `{"a":1}`), argumentsDone(0, `{"a":2}`)},
			unchangedArguments,
		},
		{
			"arguments over the limit",
			[]string{toolAdded(0), argumentsDelta(0, strings.Repeat("x", maxToolArguments+1))},
			"upstream tool arguments exceed 8 MiB",
		},

		// Finished items.
		{
			"done without an index", []string{`{"type":"response.output_item.done",
				"item":{"type":"reasoning","id":"a"}}`},
			"upstream returned incomplete Responses output metadata",
		},
		{
			"done without an item", []string{`{"type":"response.output_item.done",
				"output_index":0}`},
			"upstream returned incomplete Responses output metadata",
		},
		{
			"done for an unknown item", []string{itemDone(0, messageItem)},
			"upstream changed Responses output metadata",
		},
		{
			"done changes the item type",
			[]string{messageAdded(0), itemDone(0, `{"type":"reasoning","id":"item_1"}`)},
			"upstream changed Responses output metadata",
		},
		{
			"done changes the tool name",
			[]string{toolAdded(0), itemDone(0, strings.Replace(toolItem, "get_weather", "x", 1))},
			"upstream changed Responses tool metadata",
		},
		{
			"done changes the call ID",
			[]string{toolAdded(0), itemDone(0, strings.Replace(toolItem, "call_a", "call_b", 1))},
			"upstream changed Responses tool metadata",
		},
		{
			"done with too many content parts",
			[]string{messageAdded(0), itemDone(0, `{"type":"message","id":"item_1",
				"role":"assistant","content":[`+manyContentParts(maxContentBlocks+1)+`]}`)},
			"upstream returned an invalid Responses message",
		},
		{
			"done changes the role",
			[]string{messageAdded(0), itemDone(0, `{"type":"message","id":"item_1","role":"user"}`)},
			"upstream returned an invalid Responses message",
		},
		{
			"done with unknown content",
			[]string{messageAdded(0), itemDone(0, `{"type":"message","id":"item_1",
				"role":"assistant","content":[{"type":"other"}]}`)},
			"upstream returned unsupported Responses content",
		},
		{
			"done changes the text",
			[]string{messageAdded(0), outputTextDelta(0, "Hello world"), itemDone(0, otherText)},
			changedText,
		},
		{
			"done changes the text without changing its size",
			[]string{
				messageAdded(0), outputTextDelta(0, "Hello world"),
				itemDone(0, strings.Replace(messageItem, "Hello world", "Jello world", 1)),
			},
			changedText,
		},
		{
			"done removes content",
			[]string{messageAdded(0), outputTextDelta(0, "x"), itemDone(0, `{"type":"message",
				"id":"item_1","role":"assistant","content":[]}`)},
			"upstream Responses final content does not match its streamed content",
		},

		// Completion.
		{
			"completed without a response", []string{`{"type":"response.completed"}`},
			"upstream Responses completion is missing its response object",
		},
		{
			"completed before created", []string{responseCompleted(`[]`)},
			"upstream Responses completion arrived before response.created",
		},
		{
			"completed with another ID",
			[]string{responseCreated, `{"type":"response.completed","response":{"id":"resp_2"}}`},
			"upstream Responses completion ID does not match response.created",
		},
		{
			"empty output before all items finished",
			[]string{responseCreated, messageAdded(0), responseCompleted(`[]`)},
			"upstream Responses completion has empty output before all streamed items completed",
		},
		{
			"final output count",
			[]string{responseCreated, responseCompleted(`[` + messageItem + `]`)},
			"upstream Responses completion output count mismatch: streamed=0 final=1",
		},
		{
			"unknown incomplete reason",
			[]string{responseCreated, responseIncomplete("other", `[]`)},
			"upstream Responses request ended with an unsupported incomplete reason",
		},
		{
			"incomplete without details",
			[]string{responseCreated, `{"type":"response.incomplete",
				"response":{"id":"resp_1","status":"incomplete","output":[]}}`},
			"upstream Responses request did not complete successfully",
		},
		{
			"completed with another status",
			[]string{responseCreated, `{"type":"response.completed",
				"response":{"id":"resp_1","status":"failed","output":[]}}`},
			"upstream Responses request did not complete successfully",
		},
		{
			"invalid usage",
			[]string{responseCreated, `{"type":"response.completed","response":{"id":"resp_1",
				"status":"completed","output":[],"usage":{"input_tokens":-1}}}`},
			"upstream returned invalid token usage",
		},
		{
			"events after completion", []string{responseCreated, responseCompleted(`[]`), `{"type":"x"}`},
			"upstream sent events after response completion",
		},
		{
			"interrupted stream", []string{responseCreated, messageAdded(0)},
			"upstream Responses stream ended before response completion",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := runResponses(testCase.events)
			assertError(t, err, testCase.want)
		})
	}
}

func manyContentParts(count int) string {
	parts := make([]string, 0, count)
	for index := 0; index < count; index++ {
		parts = append(parts, `{"type":"output_text","text":"x"}`)
	}
	return strings.Join(parts, ",")
}

func TestResponsesStreamChecksTheEventName(t *testing.T) {
	cases := []struct {
		name    string
		isValid bool
	}{
		{"", true}, {"message", true}, {"response.created", true}, {"response.completed", false},
	}

	for _, testCase := range cases {
		t.Run("name "+testCase.name, func(t *testing.T) {
			translator := newResponsesStream(nil)

			_, err := translator.consume([]byte(responseCreated), testCase.name)
			if testCase.isValid {
				if err != nil {
					t.Fatalf("consume: %v", err)
				}
				return
			}
			assertError(t, err, "upstream Responses event name does not match its data")
		})
	}
}

func TestTranslateResponsesMessageAppliesStopSequences(t *testing.T) {
	const textOutput = `"content":"one STOP two END three"`
	stringPointer := func(value string) *string { return &value }

	cases := []struct {
		name         string
		stops        []string
		message      string
		finishReason string
		wantText     string
		wantReason   string
		wantSequence *string
	}{
		{
			name:         "no stop sequences",
			message:      textOutput,
			finishReason: "stop",
			wantText:     "one STOP two END three",
			wantReason:   "end_turn",
		},
		{
			name:         "no match",
			stops:        []string{"ABSENT"},
			message:      textOutput,
			finishReason: "stop",
			wantText:     "one STOP two END three",
			wantReason:   "end_turn",
		},
		{
			name:         "first marker in the text wins over list order",
			stops:        []string{"END", "STOP"},
			message:      textOutput,
			finishReason: "stop",
			wantText:     "one ",
			wantReason:   "stop_sequence",
			wantSequence: stringPointer("STOP"),
		},
		{
			name:         "marker at the start leaves empty text",
			stops:        []string{"one"},
			message:      textOutput,
			finishReason: "stop",
			wantText:     "",
			wantReason:   "stop_sequence",
			wantSequence: stringPointer("one"),
		},
		{
			name:         "refusal keeps its full text",
			stops:        []string{"no"},
			message:      `"refusal":"no way"`,
			finishReason: "stop",
			wantText:     "no way",
			wantReason:   "refusal",
		},
		{
			name:         "content filter keeps its full text",
			stops:        []string{"STOP"},
			message:      textOutput,
			finishReason: "content_filter",
			wantText:     "one STOP two END three",
			wantReason:   "refusal",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := messagesRequest{Model: "claude", StopSequences: testCase.stops}
			completion := decodeCompletion(t,
				completionWith(testCase.message, testCase.finishReason))

			result, err := translateResponsesMessage(completion, request, "msg_1")
			if err != nil {
				t.Fatalf("translateResponsesMessage: %v", err)
			}

			if len(result.Content) != 1 || *result.Content[0].Text != testCase.wantText {
				t.Errorf("content = %s, want text %q", marshal(t, result.Content), testCase.wantText)
			}
			if *result.StopReason != testCase.wantReason {
				t.Errorf("stop_reason = %q, want %q", *result.StopReason, testCase.wantReason)
			}
			switch {
			case testCase.wantSequence == nil && result.StopSequence != nil:
				t.Errorf("stop_sequence = %q, want null", *result.StopSequence)
			case testCase.wantSequence != nil && result.StopSequence == nil:
				t.Errorf("stop_sequence = null, want %q", *testCase.wantSequence)
			case testCase.wantSequence != nil && *result.StopSequence != *testCase.wantSequence:
				t.Errorf("stop_sequence = %q, want %q",
					*result.StopSequence, *testCase.wantSequence)
			}
		})
	}
}

func TestTranslateResponsesMessageRemovesToolCallsAfterStop(t *testing.T) {
	request := decodeRequest(t, declaredToolRequest)
	request.StopSequences = []string{"STOP"}
	completion := decodeCompletion(t, completionWith(`"content":"before STOP after",
		"tool_calls":[{"id":"call_1","type":"function",
		"function":{"name":"get_weather","arguments":"{}"}}]`, "tool_calls"))

	result, err := translateResponsesMessage(completion, request, "msg_1")
	if err != nil {
		t.Fatalf("translateResponsesMessage: %v", err)
	}

	assertJSONEqual(t, marshal(t, result.Content), `[{"type":"text","text":"before "}]`)
	if *result.StopReason != "stop_sequence" {
		t.Errorf("stop_reason = %q", *result.StopReason)
	}
}

func TestTranslateResponsesMessageValidatesBeforeTrimming(t *testing.T) {
	request := decodeRequest(t, declaredToolRequest)
	request.StopSequences = []string{"STOP"}
	completion := decodeCompletion(t, completionWith(`"content":"before STOP after",
		"tool_calls":[{"id":"call_1","type":"function",
		"function":{"name":"undeclared","arguments":"{}"}}]`, "tool_calls"))

	_, err := translateResponsesMessage(completion, request, "msg_1")
	assertError(t, err, "upstream returned invalid or undeclared tool metadata")
}
