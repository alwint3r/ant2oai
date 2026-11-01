package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

const (
	maxJSONDepth     = 128
	maxResponseBytes = 64 << 20
	maxEventBytes    = 2 << 20
	maxToolArguments = 8 << 20
	maxToolCalls     = 128
	maxContentBlocks = 1024
)

type messagesRequest struct {
	Model             string          `json:"model"`
	MaxTokens         int64           `json:"max_tokens"`
	Messages          []inputMessage  `json:"messages"`
	System            json.RawMessage `json:"system,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	TopK              *int            `json:"top_k,omitempty"`
	StopSequences     []string        `json:"stop_sequences,omitempty"`
	Tools             []inputTool     `json:"tools,omitempty"`
	ToolChoice        *toolChoice     `json:"tool_choice,omitempty"`
	Metadata          json.RawMessage `json:"metadata,omitempty"`
	Thinking          json.RawMessage `json:"thinking,omitempty"`
	CacheControl      json.RawMessage `json:"cache_control,omitempty"`
	OutputConfig      *outputConfig   `json:"output_config,omitempty"`
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
}

type outputConfig struct {
	Effort     json.RawMessage `json:"effort,omitempty"`
	Format     json.RawMessage `json:"format,omitempty"`
	TaskBudget json.RawMessage `json:"task_budget,omitempty"`
}

type inputMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
}

type inputBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	Source    *imageSource    `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

type inputTool struct {
	Type                string          `json:"type,omitempty"`
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	InputSchema         json.RawMessage `json:"input_schema"`
	Strict              *bool           `json:"strict,omitempty"`
	DeferLoading        bool            `json:"defer_loading,omitempty"`
	CacheControl        json.RawMessage `json:"cache_control,omitempty"`
	EagerInputStreaming bool            `json:"eager_input_streaming,omitempty"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type completionRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	MaxTokens           int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens int64          `json:"max_completion_tokens,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Tools               []functionTool `json:"tools,omitempty"`
	ToolChoice          any            `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool          `json:"parallel_tool_calls,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type functionTool struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

type functionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type completionResponse struct {
	Choices []completionChoice `json:"choices"`
	Usage   *completionUsage   `json:"usage"`
	Error   json.RawMessage    `json:"error"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	Delta        completionDelta   `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type completionMessage struct {
	Role         string          `json:"role"`
	Content      *string         `json:"content"`
	Refusal      string          `json:"refusal"`
	ToolCalls    []toolCall      `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type completionDelta struct {
	Role         string          `json:"role"`
	Content      *string         `json:"content"`
	Refusal      *string         `json:"refusal"`
	ToolCalls    []toolCallDelta `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

type toolCallDelta struct {
	Index    *int         `json:"index"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type completionUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type messageResponse struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Content      []outputBlock `json:"content"`
	Model        string        `json:"model"`
	StopReason   *string       `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        messageUsage  `json:"usage"`
}

type outputBlock struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type messageUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type errorResponse struct {
	Type      string   `json:"type"`
	Error     apiError `json:"error"`
	RequestID string   `json:"request_id"`
}

type messageEvent struct {
	Type         string           `json:"type"`
	Message      *messageResponse `json:"message,omitempty"`
	Index        *int             `json:"index,omitempty"`
	ContentBlock *outputBlock     `json:"content_block,omitempty"`
	Delta        any              `json:"delta,omitempty"`
	Usage        *messageUsage    `json:"usage,omitempty"`
	Error        *apiError        `json:"error,omitempty"`
}

func hasValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

func isJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) != 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// encoding/json validates syntax separately. This pass bounds nesting even in
// raw tool schemas and arguments that the typed decoder does not traverse.
func checkJSONDepth(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must use valid UTF-8")
	}

	depth := 0
	inString := false
	escaped := false
	for _, character := range data {
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxJSONDepth {
				return errors.New("JSON nesting exceeds 128 levels")
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}
