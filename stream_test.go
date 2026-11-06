package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func textChunk(text string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%q}}]}`, text)
}

func finishChunk(reason string) string {
	return fmt.Sprintf(`{"choices":[{"index":0,"delta":{},"finish_reason":%q}]}`, reason)
}

// toolChunk returns one tool call fragment. Empty fields stay out of the delta.
func toolChunk(index int, id, name, arguments string) string {
	fields := fmt.Sprintf(`"index":%d`, index)
	if id != "" {
		fields += fmt.Sprintf(`,"id":%q,"type":"function"`, id)
	}
	function, err := json.Marshal(map[string]string{"name": name, "arguments": arguments})
	if err != nil {
		panic(err)
	}
	return `{"choices":[{"index":0,"delta":{"tool_calls":[{` + fields +
		`,"function":` + string(function) + `}]}}]}`
}

func blockStart(index int, block string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, block)
}

func textStart(index int) string {
	return blockStart(index, `{"type":"text","text":""}`)
}

func toolStart(index int, id, name string) string {
	return blockStart(index,
		fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name))
}

func textDelta(index int, text string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,
		"delta":{"type":"text_delta","text":%q}}`, index, text)
}

func argumentDelta(index int, arguments string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,
		"delta":{"type":"input_json_delta","partial_json":%q}}`, index, arguments)
}

func blockStop(index int) string {
	return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)
}

func messageEnd(stopReason, usage string) []string {
	return []string{
		`{"type":"message_delta","delta":{"stop_reason":"` + stopReason +
			`","stop_sequence":null},"usage":` + usage + `}`,
		`{"type":"message_stop"}`,
	}
}

// runChunks feeds chunks in order and then finishes the stream. It stops at the
// first error, as the HTTP layer does.
func runChunks(t *testing.T, chunks []string) ([]messageEvent, error) {
	t.Helper()

	translator := newStreamTranslator([]inputTool{{Name: "get_weather"}, {Name: "search"}})
	var events []messageEvent
	for _, source := range chunks {
		chunkEvents, err := translator.consume(decodeCompletion(t, source))
		if err != nil {
			return nil, err
		}
		events = append(events, chunkEvents...)
	}

	finalEvents, err := translator.finish()
	if err != nil {
		return nil, err
	}
	return append(events, finalEvents...), nil
}

func TestStreamTranslator(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{
			name: "text with usage",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
				textChunk("Hel"), textChunk("lo"), finishChunk("stop"),
				`{"choices":[],"usage":` + testUsage + `}`,
			},
			want: append([]string{textStart(0), textDelta(0, "Hel"), textDelta(0, "lo"),
				blockStop(0)}, messageEnd("end_turn", translatedTestUsage)...),
		},
		{
			name:   "empty reply has one empty text block",
			chunks: []string{finishChunk("stop")},
			want: append([]string{textStart(0), textDelta(0, ""), blockStop(0)},
				messageEnd("end_turn", emptyUsage)...),
		},
		{
			name:   "length",
			chunks: []string{textChunk("x"), finishChunk("length")},
			want: append([]string{textStart(0), textDelta(0, "x"), blockStop(0)},
				messageEnd("max_tokens", emptyUsage)...),
		},
		{
			name:   "content filter",
			chunks: []string{textChunk("x"), finishChunk("content_filter")},
			want: append([]string{textStart(0), textDelta(0, "x"), blockStop(0)},
				messageEnd("refusal", emptyUsage)...),
		},
		{
			name: "refusal",
			chunks: []string{
				`{"choices":[{"index":0,"delta":{"refusal":"no"}}]}`, finishChunk("stop"),
			},
			want: append([]string{textStart(0), textDelta(0, "no"), blockStop(0)},
				messageEnd("refusal", emptyUsage)...),
		},
		{
			// The first tool streams at once. The second tool and later text wait
			// for finish, because downstream blocks must stay sequential.
			name: "text, parallel tools, and later text",
			chunks: []string{
				textChunk("calling"),
				toolChunk(0, "call_a", "get_weather", ""),
				toolChunk(0, "", "", `{"ci`),
				toolChunk(1, "call_b", "search", `{}`),
				toolChunk(0, "", "", `ty":"x"}`),
				textChunk("after"),
				finishChunk("tool_calls"),
			},
			want: append([]string{
				textStart(0), textDelta(0, "calling"), blockStop(0),
				toolStart(1, "call_a", "get_weather"),
				argumentDelta(1, `{"ci`), argumentDelta(1, `ty":"x"}`), blockStop(1),
				toolStart(2, "call_b", "search"), argumentDelta(2, `{}`), blockStop(2),
				textStart(3), textDelta(3, "after"), blockStop(3),
			}, messageEnd("tool_use", emptyUsage)...),
		},
		{
			name: "tool metadata split across chunks",
			chunks: []string{
				toolChunk(0, "call_", "sea", ""),
				toolChunk(0, "a", "rch", `{}`),
				finishChunk("stop"),
			},
			want: append([]string{
				toolStart(0, "call_a", "search"), argumentDelta(0, `{}`), blockStop(0),
			}, messageEnd("tool_use", emptyUsage)...),
		},
		{
			name: "tool without arguments is replayed at the end",
			chunks: []string{
				toolChunk(0, "call_a", "search", ""), finishChunk("tool_calls"),
			},
			want: append([]string{toolStart(0, "call_a", "search"), blockStop(0)},
				messageEnd("tool_use", emptyUsage)...),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events, err := runChunks(t, testCase.chunks)
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			assertEvents(t, events, testCase.want)
		})
	}
}

func TestStreamTranslatorRejectsInvalidChunks(t *testing.T) {
	stop := finishChunk("stop")
	longID := strings.Repeat("i", 257)
	const invalidIndex = "upstream sent an invalid tool call index"
	const invalidMetadata = "upstream returned invalid or undeclared tool metadata"

	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"error chunk", []string{`{"error":{"message":"x"}}`}, "upstream sent a streaming error"},
		{
			"invalid usage", []string{`{"choices":[],"usage":{"prompt_tokens":-1}}`},
			"upstream returned invalid token usage",
		},
		{
			"two choices",
			[]string{`{"choices":[{"index":0,"delta":{}},{"index":1,"delta":{}}]}`},
			"upstream sent unexpected streaming choices",
		},
		{
			"choice index", []string{`{"choices":[{"index":1,"delta":{}}]}`},
			"upstream sent unexpected streaming choices",
		},
		{
			"choice after the finish reason", []string{stop, textChunk("late")},
			"upstream sent unexpected streaming choices",
		},
		{
			"delta role", []string{`{"choices":[{"index":0,"delta":{"role":"user"}}]}`},
			"upstream sent an unsupported streaming delta",
		},
		{
			"legacy function call",
			[]string{`{"choices":[{"index":0,"delta":{"function_call":{"name":"a"}}}]}`},
			"upstream sent an unsupported streaming delta",
		},
		{
			"tool without an index",
			[]string{`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"a"}]}}]}`},
			invalidIndex,
		},
		{"negative tool index", []string{toolChunk(-1, "a", "search", "")}, invalidIndex},
		{"tool index over the limit", []string{toolChunk(128, "a", "search", "")}, invalidIndex},
		{
			"tool type",
			[]string{`{"choices":[{"index":0,"delta":{"tool_calls":[
				{"index":0,"type":"other"}]}}]}`},
			"upstream sent an unsupported tool call type",
		},
		{
			"tool ID changes after it started",
			[]string{toolChunk(0, "a", "search", "{"), toolChunk(0, "b", "", "}")},
			"upstream changed tool metadata after argument streaming began",
		},
		{
			"tool name changes after it started",
			[]string{toolChunk(0, "a", "search", "{"), toolChunk(0, "", "get_weather", "}")},
			"upstream changed tool metadata after argument streaming began",
		},
		{
			"tool ID over the limit", []string{toolChunk(0, longID, "search", "")},
			"upstream tool metadata exceeds the supported limits",
		},
		{
			"tool name over the limit",
			[]string{toolChunk(0, "a", strings.Repeat("n", 65), "")},
			"upstream tool metadata exceeds the supported limits",
		},
		{
			"tool arguments over the limit",
			[]string{toolChunk(0, "a", "search", strings.Repeat("x", maxToolArguments+1))},
			"upstream tool arguments exceed 8 MiB",
		},
		{
			"empty finish reason",
			[]string{`{"choices":[{"index":0,"delta":{},"finish_reason":""}]}`},
			"upstream sent an empty finish reason",
		},
		{
			"interrupted before the finish reason", []string{textChunk("cut")},
			"upstream stream ended before a finish reason",
		},
		{
			"unknown finish reason", []string{finishChunk("other")},
			"upstream returned an unsupported finish reason",
		},
		{
			"tool_calls reason without tools", []string{finishChunk("tool_calls")},
			"upstream finished with tool_calls without any tool calls",
		},
		{
			"undeclared tool", []string{toolChunk(0, "a", "other", "{}"), stop},
			invalidMetadata,
		},
		{"tool without an ID", []string{toolChunk(0, "", "search", "{}"), stop}, invalidMetadata},
		{
			"duplicate tool ID",
			[]string{toolChunk(0, "a", "search", "{}"), toolChunk(1, "a", "search", "{}"), stop},
			invalidMetadata,
		},
		{
			"incomplete tool arguments", []string{toolChunk(0, "a", "search", `{"x`), stop},
			"upstream tool arguments must be a complete JSON object within the nesting limit",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := runChunks(t, testCase.chunks)
			assertError(t, err, testCase.want)
		})
	}
}

// collectFrames reads an SSE body to its end and returns every frame.
func collectFrames(body io.Reader) []sseFrame {
	frames := make(chan sseFrame, 1)
	go readSSE(context.Background(), body, frames)

	var collected []sseFrame
	for frame := range frames {
		collected = append(collected, frame)
	}
	return collected
}

func TestReadSSE(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []sseFrame
	}{
		{name: "empty body"},
		{
			name: "one event",
			body: "event: update\ndata: {}\n\n",
			want: []sseFrame{{name: "update", data: []byte("{}")}},
		},
		{
			name: "several data lines are joined",
			body: "data: a\ndata: b\n\n",
			want: []sseFrame{{data: []byte("a\nb")}},
		},
		{
			name: "byte order mark on the first line",
			body: "\xef\xbb\xbfdata: a\n\n",
			want: []sseFrame{{data: []byte("a")}},
		},
		{
			name: "value without a leading space",
			body: "data:a\n\n",
			want: []sseFrame{{data: []byte("a")}},
		},
		{
			// A comment produces an empty frame, which restarts the idle timer.
			name: "comment",
			body: ": keep-alive\ndata: a\n\n",
			want: []sseFrame{{}, {data: []byte("a")}},
		},
		{
			name: "unknown fields are ignored",
			body: "id: 1\nretry: 5\ndata: a\n\n",
			want: []sseFrame{{data: []byte("a")}},
		},
		{
			name: "final event without a blank line",
			body: "event: last\ndata: a",
			want: []sseFrame{{name: "last", data: []byte("a")}},
		},
		{
			name: "event name resets after each event",
			body: "event: first\ndata: a\n\ndata: b\n\n",
			want: []sseFrame{{name: "first", data: []byte("a")}, {data: []byte("b")}},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := collectFrames(strings.NewReader(testCase.body))

			if len(got) != len(testCase.want) {
				t.Fatalf("got %d frames, want %d: %+v", len(got), len(testCase.want), got)
			}
			for index, frame := range got {
				want := testCase.want[index]
				if frame.name != want.name || string(frame.data) != string(want.data) {
					t.Errorf("frame %d = %q/%q, want %q/%q",
						index, frame.name, frame.data, want.name, want.data)
				}
				if frame.err != nil {
					t.Errorf("frame %d error: %v", index, frame.err)
				}
			}
		})
	}
}

func TestReadSSEReportsFailures(t *testing.T) {
	const readFailure = "upstream stream read failed or exceeded its size limit"
	event := "data: " + strings.Repeat("x", maxEventBytes/2) + "\n"

	cases := []struct {
		name string
		body io.Reader
		want string
	}{
		{"read error", io.MultiReader(strings.NewReader("data: a\n"), failingReader{}), readFailure},
		{"line over the limit", strings.NewReader(strings.Repeat("x", maxEventBytes+2)), readFailure},
		{
			"event over the limit",
			strings.NewReader(event + event + event),
			"upstream SSE event exceeds 2 MiB",
		},
		{
			"stream over the limit",
			repeatedFrames(maxResponseBytes),
			"upstream stream exceeds 64 MiB",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			frames := collectFrames(testCase.body)
			if len(frames) == 0 {
				t.Fatal("got no frames, want a final error frame")
			}
			assertError(t, frames[len(frames)-1].err, testCase.want)
		})
	}
}

// repeatedFrames returns more than minimumBytes of small, valid SSE frames.
func repeatedFrames(minimumBytes int) io.Reader {
	frame := ": " + strings.Repeat("x", 4094) + "\n\n"
	return strings.NewReader(strings.Repeat(frame, minimumBytes/len(frame)+2))
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestReadSSEStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	frames := make(chan sseFrame)
	done := make(chan struct{})
	go func() {
		defer close(done)
		readSSE(ctx, strings.NewReader("data: a\n\ndata: b\n\n"), frames)
	}()

	// Nobody receives the second frame, so only cancellation can release the reader.
	first := <-frames
	if string(first.data) != "a" {
		t.Fatalf("first frame = %q", first.data)
	}
	cancel()
	<-done
}
