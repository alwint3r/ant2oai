package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testProxyKey = "proxy-secret"

// chatStream is a complete Chat Completions stream with one text message.
func chatStream() string {
	return sseBody(textChunk("Hello"), finishChunk("stop")) + "data: [DONE]\n\n"
}

// responsesStreamBody is a complete Responses stream with one text message.
func responsesStreamBody() string {
	return namedSSEBody(textResponse()...)
}

func keyedProxy() *proxy {
	cfg := testConfig()
	cfg.proxyAPIKey = testProxyKey
	return newTestProxy(cfg, answerWith(http.StatusOK, nil, simpleCompletion))
}

func TestProbesNeedNoCredential(t *testing.T) {
	application := keyedProxy()

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"health GET", http.MethodGet, "/healthz", http.StatusOK, `{"status":"ok"}` + "\n"},
		{"health HEAD", http.MethodHead, "/healthz", http.StatusOK, `{"status":"ok"}` + "\n"},
		{"hello HEAD", http.MethodHead, "/api/hello", http.StatusNoContent, ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			recorder := serve(application, request)

			if recorder.Code != testCase.wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, testCase.wantStatus)
			}
			if recorder.Body.String() != testCase.wantBody {
				t.Errorf("body = %q, want %q", recorder.Body, testCase.wantBody)
			}
		})
	}
}

func TestProxyKey(t *testing.T) {
	const invalidKey = "Invalid proxy credential; " +
		"set ANTHROPIC_AUTH_TOKEN to the configured proxy key."

	cases := []struct {
		name    string
		headers map[string]string
		isValid bool
	}{
		{"no credential", nil, false},
		{"API key header", map[string]string{"X-Api-Key": testProxyKey}, true},
		{"bearer token", map[string]string{"Authorization": "Bearer " + testProxyKey}, true},
		{"bearer scheme ignores case", map[string]string{"Authorization": "bearer " + testProxyKey}, true},
		{"wrong API key", map[string]string{"X-Api-Key": "wrong"}, false},
		{"wrong bearer token", map[string]string{"Authorization": "Bearer wrong"}, false},
		{"other scheme", map[string]string{"Authorization": "Basic " + testProxyKey}, false},
		{"scheme only", map[string]string{"Authorization": "Bearer"}, false},
		{
			"one valid credential is enough",
			map[string]string{"X-Api-Key": "wrong", "Authorization": "Bearer " + testProxyKey},
			true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := newMessagesRequest(context.Background(), simpleRequest)
			for name, value := range testCase.headers {
				request.Header.Set(name, value)
			}

			recorder := serve(keyedProxy(), request)
			if testCase.isValid {
				if recorder.Code != http.StatusOK {
					t.Errorf("status = %d; body: %s", recorder.Code, recorder.Body)
				}
				return
			}
			assertAPIError(t, recorder, http.StatusUnauthorized, "authentication_error", invalidKey)
		})
	}
}

func TestCredentialIsCheckedBeforeRouting(t *testing.T) {
	recorder := serve(keyedProxy(), httptest.NewRequest(http.MethodPost, "/unknown", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestRouting(t *testing.T) {
	const jsonType = "application/json"

	cases := []struct {
		name        string
		method      string
		path        string
		contentType string
		wantStatus  int
		wantKind    string
		wantMessage string
	}{
		{
			"token counting", http.MethodPost, "/v1/messages/count_tokens", jsonType,
			http.StatusNotFound, "not_found_error",
			"Exact token counting is unavailable for this upstream. " +
				"Claude Code can use its local estimate.",
		},
		{
			"unknown path", http.MethodPost, "/v1/other", jsonType, http.StatusNotFound,
			"not_found_error", "Unknown endpoint. Use POST /v1/messages.",
		},
		{
			"wrong method", http.MethodGet, "/v1/messages", jsonType,
			http.StatusMethodNotAllowed, "invalid_request_error", "This endpoint requires POST.",
		},
		{
			"missing content type", http.MethodPost, "/v1/messages", "",
			http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json.",
		},
		{
			"wrong content type", http.MethodPost, "/v1/messages", "text/plain",
			http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json.",
		},
		{
			"invalid content type", http.MethodPost, "/v1/messages", ";;;",
			http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json.",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(http.StatusOK, nil, simpleCompletion)
			application := newTestProxy(testConfig(), upstream)
			request := httptest.NewRequest(testCase.method, testCase.path,
				strings.NewReader(simpleRequest))
			if testCase.contentType != "" {
				request.Header.Set("Content-Type", testCase.contentType)
			}

			recorder := serve(application, request)
			assertAPIError(t, recorder, testCase.wantStatus, testCase.wantKind, testCase.wantMessage)
			if len(upstream.recorded()) != 0 {
				t.Error("a rejected request reached the upstream")
			}
		})
	}
}

func TestWrongMethodReportsAllowedMethod(t *testing.T) {
	application := newTestProxy(testConfig(), &fakeUpstream{})
	recorder := serve(application, httptest.NewRequest(http.MethodGet, "/v1/messages", nil))

	assertHeader(t, recorder.Header(), "Allow", http.MethodPost)
}

func TestSuccessfulResponse(t *testing.T) {
	application := newTestProxy(testConfig(), answerWith(http.StatusOK, nil, simpleCompletion))
	recorder := postMessages(application, simpleRequest)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", recorder.Code, recorder.Body)
	}
	assertHeader(t, recorder.Header(), "Content-Type", "application/json")
	assertHeader(t, recorder.Header(), "X-Content-Type-Options", "nosniff")
	assertHeader(t, recorder.Header(), "X-Ant2oai-Usage", "unavailable")

	requestID := recorder.Header().Get("request-id")
	if !strings.HasPrefix(requestID, "req_") || len(requestID) != len("req_")+32 {
		t.Errorf("request-id = %q, want req_ and 32 hexadecimal digits", requestID)
	}

	var message messageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if message.ID != "msg_"+strings.TrimPrefix(requestID, "req_") {
		t.Errorf("message ID = %q does not match request ID %q", message.ID, requestID)
	}
	if message.Model != "claude" {
		t.Errorf("model = %q, want the model from the request", message.Model)
	}
}

func TestReportedUsageHasNoUnavailableHeader(t *testing.T) {
	completion := `{"usage":` + testUsage + `,"choices":[{"index":0,"finish_reason":"stop",
		"message":{"role":"assistant","content":"x"}}]}`
	application := newTestProxy(testConfig(), answerWith(http.StatusOK, nil, completion))

	recorder := postMessages(application, simpleRequest)
	assertHeader(t, recorder.Header(), "X-Ant2oai-Usage", "")
}

func TestRequestBodyValidation(t *testing.T) {
	const invalidRequest = "invalid_request_error"
	const unsupportedRequest = "Expected one valid Messages request with supported fields. " +
		"Disable experimental betas and check the compatibility guide in README.md."

	cases := []struct {
		name        string
		body        string
		wantStatus  int
		wantKind    string
		wantMessage string
	}{
		{
			"body over the limit", strings.Repeat(" ", 2<<20), http.StatusRequestEntityTooLarge,
			"request_too_large", "Request body exceeds max-body-bytes.",
		},
		{
			"deep JSON", strings.Repeat("[", 129) + strings.Repeat("]", 129),
			http.StatusBadRequest, invalidRequest, "JSON nesting exceeds 128 levels",
		},
		{
			"invalid UTF-8", "{\"model\":\"\xff\"}", http.StatusBadRequest, invalidRequest,
			"JSON must use valid UTF-8",
		},
		{"empty body", "", http.StatusBadRequest, invalidRequest, unsupportedRequest},
		{"malformed JSON", "{", http.StatusBadRequest, invalidRequest, unsupportedRequest},
		{
			"unknown field", `{"secret-field":1}`, http.StatusBadRequest, invalidRequest,
			unsupportedRequest,
		},
		{
			"trailing data", simpleRequest + `{}`, http.StatusBadRequest, invalidRequest,
			unsupportedRequest,
		},
		{
			"safeguards field", `{"safeguards":{}}`, http.StatusBadRequest, invalidRequest,
			"safeguards: server-side auto-mode safety checks are unsupported. " +
				"Set CLAUDE_CODE_AUTO_MODE_SERVER=0 and " +
				"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1, then start a new session.",
		},
		{
			"translation failure", `{"model":"m","max_tokens":0,"messages":[]}`,
			http.StatusBadRequest, invalidRequest, "max_tokens must be positive",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := &fakeUpstream{}
			recorder := postMessages(newTestProxy(testConfig(), upstream), testCase.body)

			assertAPIError(t, recorder, testCase.wantStatus, testCase.wantKind, testCase.wantMessage)
			if len(upstream.recorded()) != 0 {
				t.Error("an invalid request reached the upstream")
			}
		})
	}
}

func TestUnreadableRequestBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", failingReader{})
	request.Header.Set("Content-Type", "application/json")

	recorder := serve(newTestProxy(testConfig(), &fakeUpstream{}), request)
	assertAPIError(t, recorder, http.StatusBadRequest, "invalid_request_error",
		"Could not read the request body.")
}

func TestUpstreamRequest(t *testing.T) {
	cases := []struct {
		name       string
		cfg        func() config
		body       string
		reply      string
		wantURL    string
		wantAccept string
		wantAuth   string
	}{
		{
			name: "chat completion", cfg: testConfig, body: simpleRequest,
			reply:      simpleCompletion,
			wantURL:    "https://upstream.test/v1/chat/completions",
			wantAccept: "application/json", wantAuth: "Bearer " + testUpstreamKey,
		},
		{
			name: "chat stream", cfg: testConfig, body: streamingRequest, reply: chatStream(),
			wantURL:    "https://upstream.test/v1/chat/completions",
			wantAccept: "text/event-stream", wantAuth: "Bearer " + testUpstreamKey,
		},
		{
			name: "no upstream key",
			cfg: func() config {
				cfg := testConfig()
				cfg.upstreamAPIKey = ""
				return cfg
			},
			body: simpleRequest, reply: simpleCompletion,
			wantURL:    "https://upstream.test/v1/chat/completions",
			wantAccept: "application/json",
		},
		{
			name: "ChatGPT buffers through a stream", cfg: chatGPTTestConfig, body: simpleRequest,
			reply:      responsesStreamBody(),
			wantURL:    "https://api.openai.com/v1/responses",
			wantAccept: "text/event-stream", wantAuth: "Bearer access-1",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWithStream(testCase.reply)
			if testCase.reply == simpleCompletion {
				upstream = answerWith(http.StatusOK, nil, testCase.reply)
			}
			application := newTestProxy(testCase.cfg(), upstream)

			// Inbound credentials and metadata must stay inside the proxy.
			request := newMessagesRequest(context.Background(), testCase.body)
			request.Header.Set("Authorization", "Bearer inbound-secret")
			request.Header.Set("X-Api-Key", "inbound-secret")
			request.Header.Set("Cookie", "session=inbound-secret")
			request.Header.Set("Anthropic-Beta", "inbound-beta")
			recorder := serve(application, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d; body: %s", recorder.Code, recorder.Body)
			}

			calls := upstream.recorded()
			if len(calls) != 1 {
				t.Fatalf("got %d upstream calls, want 1", len(calls))
			}
			call := calls[0]
			if call.method != http.MethodPost || call.url != testCase.wantURL {
				t.Errorf("request = %s %s, want POST %s", call.method, call.url, testCase.wantURL)
			}
			assertHeader(t, call.header, "Content-Type", "application/json")
			assertHeader(t, call.header, "Accept", testCase.wantAccept)
			assertHeader(t, call.header, "Authorization", testCase.wantAuth)
			for _, name := range []string{"X-Api-Key", "Cookie", "Anthropic-Beta"} {
				assertHeader(t, call.header, name, "")
			}
		})
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	const invalidRequest = "invalid_request_error"
	const rejected = "Upstream rejected the translated request. " +
		"Check token limits, sampling parameters, tool schemas, and model support."
	const contextExceeded = "Prompt is too long: upstream context window exceeded. " +
		"Compact the conversation or select a larger-context model."
	const contextBody = `{"error":{"code":"context_length_exceeded"}}`

	cases := []struct {
		name        string
		status      int
		body        string
		wantStatus  int
		wantKind    string
		wantMessage string
		wantNoRetry bool
	}{
		{name: "bad request", status: 400, wantStatus: 400, wantKind: invalidRequest, wantMessage: rejected},
		{name: "unprocessable", status: 422, wantStatus: 400, wantKind: invalidRequest, wantMessage: rejected},
		{
			name: "context window", status: 400, body: contextBody, wantStatus: 400,
			wantKind: invalidRequest, wantMessage: contextExceeded,
		},
		{
			name: "provider text stays hidden", status: 400,
			body:       `{"error":{"code":"other","message":"prompt fragment"}}`,
			wantStatus: 400, wantKind: invalidRequest, wantMessage: rejected,
		},
		{
			name: "unauthorized", status: 401, wantStatus: 502, wantKind: "api_error",
			wantMessage: "Upstream authentication or permission check failed. " +
				"Check OPENAI_API_KEY and model access.",
			wantNoRetry: true,
		},
		{
			name: "forbidden", status: 403, wantStatus: 502, wantKind: "api_error",
			wantMessage: "Upstream authentication or permission check failed. " +
				"Check OPENAI_API_KEY and model access.",
			wantNoRetry: true,
		},
		{
			name: "not found", status: 404, wantStatus: 400, wantKind: invalidRequest,
			wantMessage: "Upstream endpoint or model was not found. " +
				"Check OPENAI_BASE_URL (including its API prefix) and model mapping.",
		},
		{
			name: "rate limit", status: 429, wantStatus: 429, wantKind: "rate_limit_error",
			wantMessage: "Upstream rate limit reached.",
		},
		{
			name: "request timeout", status: 408, wantStatus: 504, wantKind: "api_error",
			wantMessage: "Upstream request timed out.",
		},
		{
			name: "gateway timeout", status: 504, wantStatus: 504, wantKind: "api_error",
			wantMessage: "Upstream request timed out.",
		},
		{
			name: "server error", status: 500, wantStatus: 502, wantKind: "api_error",
			wantMessage: "Upstream returned HTTP 500. Check its configuration and availability.",
		},
		{
			name: "redirect is not followed", status: 302, wantStatus: 502, wantKind: "api_error",
			wantMessage: "Upstream returned HTTP 302. Check its configuration and availability.",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(testCase.status, nil, testCase.body)
			recorder := postMessages(newTestProxy(testConfig(), upstream), simpleRequest)

			assertAPIError(t, recorder, testCase.wantStatus, testCase.wantKind, testCase.wantMessage)
			wantRetry := ""
			if testCase.wantNoRetry {
				wantRetry = "false"
			}
			assertHeader(t, recorder.Header(), "X-Should-Retry", wantRetry)
			if recorder.Header().Get("X-Ant2oai-Upstream-Status") == "" {
				t.Error("the upstream status header is missing")
			}
		})
	}
}

func TestRetryHeaders(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		headers         map[string]string
		wantShouldRetry string
		wantRetryAfter  string
	}{
		{name: "none", status: 500},
		{
			name: "retry true", status: 500, headers: map[string]string{"X-Should-Retry": "true"},
			wantShouldRetry: "true",
		},
		{
			name: "retry false", status: 500, headers: map[string]string{"X-Should-Retry": "false"},
			wantShouldRetry: "false",
		},
		{
			name: "other retry value", status: 500, headers: map[string]string{"X-Should-Retry": "1"},
		},
		{
			name: "authentication failure never retries", status: 401,
			headers:         map[string]string{"X-Should-Retry": "true"},
			wantShouldRetry: "false",
		},
		{
			name: "seconds", status: 429, headers: map[string]string{"Retry-After": "5"},
			wantRetryAfter: "5",
		},
		{
			name: "one day", status: 429, headers: map[string]string{"Retry-After": "86400"},
			wantRetryAfter: "86400",
		},
		{name: "over one day", status: 429, headers: map[string]string{"Retry-After": "86401"}},
		{name: "negative", status: 429, headers: map[string]string{"Retry-After": "-1"}},
		{name: "unknown format", status: 429, headers: map[string]string{"Retry-After": "soon"}},
		{
			name: "date in the past", status: 429,
			headers: map[string]string{"Retry-After": "Wed, 21 Oct 2015 07:28:00 GMT"},
		},
		{
			name: "date too far ahead", status: 429,
			headers: map[string]string{"Retry-After": "Fri, 01 Jan 2100 00:00:00 GMT"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(testCase.status, testCase.headers, "")
			recorder := postMessages(newTestProxy(testConfig(), upstream), simpleRequest)

			assertHeader(t, recorder.Header(), "X-Should-Retry", testCase.wantShouldRetry)
			assertHeader(t, recorder.Header(), "Retry-After", testCase.wantRetryAfter)
		})
	}
}

func TestChatGPTErrorMapping(t *testing.T) {
	const generic = "ChatGPT request failed. Check account permissions, model access, " +
		"OpenAI availability, and the upstream status and request-ID headers."
	const limitBody = `{"error":{"code":"subscription_sharing_usage_limit_exceeded"}}`
	const limitMessage = "ChatGPT app or plan usage limit reached " +
		"(subscription_sharing_usage_limit_exceeded). " +
		"Review https://chatgpt.com/settings/usage before sending more requests."

	cases := []struct {
		name            string
		status          int
		body            string
		wantStatus      int
		wantKind        string
		wantMessage     string
		wantCode        string
		wantShouldRetry string
	}{
		{
			name: "bad request", status: 400, wantStatus: 400, wantKind: "invalid_request_error",
			wantMessage: generic, wantShouldRetry: "false",
		},
		{
			name: "not found", status: 404, wantStatus: 404, wantKind: "invalid_request_error",
			wantMessage: generic,
		},
		{
			name: "unprocessable", status: 422, wantStatus: 422, wantKind: "invalid_request_error",
			wantMessage: generic,
		},
		{
			name: "unauthorized", status: 401, wantStatus: 401, wantKind: "authentication_error",
			wantMessage: generic, wantShouldRetry: "false",
		},
		{
			name: "forbidden", status: 403, wantStatus: 403, wantKind: "permission_error",
			wantMessage: generic, wantShouldRetry: "false",
		},
		{
			name: "rate limit", status: 429, wantStatus: 429, wantKind: "rate_limit_error",
			wantMessage: generic,
		},
		{
			name: "plan limit never retries", status: 429, body: limitBody, wantStatus: 429,
			wantKind: "rate_limit_error", wantMessage: limitMessage,
			wantCode:        "subscription_sharing_usage_limit_exceeded",
			wantShouldRetry: "false",
		},
		{
			name: "server error", status: 503, wantStatus: 503, wantKind: "api_error",
			wantMessage: generic,
		},
		{
			name: "status outside the error range", status: 302, wantStatus: 502,
			wantKind: "api_error", wantMessage: generic,
		},
		{
			name: "unknown code stays hidden", status: 500,
			body:       `{"error":{"code":"private_code","message":"prompt fragment"}}`,
			wantStatus: 500, wantKind: "api_error", wantMessage: generic,
		},
		{
			name: "deep body is ignored", status: 500, body: strings.Repeat("[", 200),
			wantStatus: 500, wantKind: "api_error", wantMessage: generic,
		},
		{
			name: "body over the limit is ignored", status: 429,
			body:       strings.Repeat(" ", 64<<10) + limitBody,
			wantStatus: 429, wantKind: "rate_limit_error", wantMessage: generic,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(testCase.status, nil, testCase.body)
			recorder := postMessages(newTestProxy(chatGPTTestConfig(), upstream), simpleRequest)

			assertAPIError(t, recorder, testCase.wantStatus, testCase.wantKind, testCase.wantMessage)
			assertHeader(t, recorder.Header(), "X-Ant2oai-Upstream-Code", testCase.wantCode)
			assertHeader(t, recorder.Header(), "X-Should-Retry", testCase.wantShouldRetry)
		})
	}
}

func TestChatGPTKnownFailureCodes(t *testing.T) {
	codes := []string{
		"subscription_sharing_user_not_eligible", "subscription_sharing_usage_limit_exceeded",
		"subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable",
		"subscription_sharing_unsupported_capability", "subscription_sharing_route_not_supported",
		"subscription_sharing_invalid_user", "chatpass_v2_scope_not_authorized",
		"chatpass_v2_invalid_authorization_context", "context_length_exceeded",
	}

	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			if _, isKnown := chatGPTFailureMessage(code); !isKnown {
				t.Error("the code is not known")
			}
		})
	}
}

func TestUpstreamRequestIDHeader(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{"valid", "req_abc-1_Z", "req_abc-1_Z"},
		{"prefix only", "req_", ""},
		{"wrong prefix", "abc_1", ""},
		{"unsafe character", "req_a b", ""},
		{"over the limit", "req_" + strings.Repeat("a", 125), ""},
		{"at the limit", "req_" + strings.Repeat("a", 124), "req_" + strings.Repeat("a", 124)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(http.StatusTooManyRequests,
				map[string]string{"X-Request-Id": testCase.id}, "")

			recorder := postMessages(newTestProxy(chatGPTTestConfig(), upstream), simpleRequest)
			assertHeader(t, recorder.Header(), "X-Ant2oai-Upstream-Request-Id", testCase.want)
		})
	}
}

func TestChatGPTCredentialFailures(t *testing.T) {
	t.Run("no session", func(t *testing.T) {
		upstream := &fakeUpstream{}
		application := newTestProxy(chatGPTTestConfig(), upstream)
		application.oauth = nil

		recorder := postMessages(application, simpleRequest)
		assertAPIError(t, recorder, http.StatusServiceUnavailable, "api_error",
			"ChatGPT credentials are unavailable; stop the proxy and run ant2oai login.")
	})

	t.Run("signed out", func(t *testing.T) {
		upstream := &fakeUpstream{}
		application := newTestProxy(chatGPTTestConfig(), upstream)
		application.oauth.store = testStore(testCredentials().withoutTokens())

		recorder := postMessages(application, simpleRequest)
		assertAPIError(t, recorder, http.StatusServiceUnavailable, "api_error",
			"ChatGPT credentials are unavailable; stop the proxy and run ant2oai login")
		assertHeader(t, recorder.Header(), "X-Should-Retry", "false")
		if len(upstream.recorded()) != 0 {
			t.Error("a request without a credential reached the upstream")
		}
	})
}

type temporaryTimeout struct{}

func (temporaryTimeout) Error() string   { return "dial timeout" }
func (temporaryTimeout) Timeout() bool   { return true }
func (temporaryTimeout) Temporary() bool { return true }

func TestTransportFailures(t *testing.T) {
	const noConnection = "Could not connect to the upstream. " +
		"Check the base URL, network, and TLS configuration."

	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{"connection failure", errors.New("refused"), http.StatusBadGateway, noConnection},
		{
			"network timeout", temporaryTimeout{}, http.StatusGatewayTimeout,
			"Upstream request timed out.",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
				return nil, testCase.err
			}}

			recorder := postMessages(newTestProxy(testConfig(), upstream), simpleRequest)
			assertAPIError(t, recorder, testCase.wantStatus, "api_error", testCase.wantMessage)
		})
	}
}

func TestRequestDeadlineBeforeUpstreamReply(t *testing.T) {
	cfg := testConfig()
	cfg.requestTimeout = time.Millisecond
	upstream := &fakeUpstream{respond: func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}

	recorder := postMessages(newTestProxy(cfg, upstream), simpleRequest)
	assertAPIError(t, recorder, http.StatusGatewayTimeout, "api_error",
		"Upstream request timed out.")
}

func TestClientDisconnectCancelsUpstreamWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &fakeUpstream{respond: func(request *http.Request) (*http.Response, error) {
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	}}

	recorder := serve(newTestProxy(testConfig(), upstream),
		newMessagesRequest(ctx, simpleRequest))
	if recorder.Body.Len() != 0 {
		t.Errorf("wrote %q to a disconnected client", recorder.Body)
	}
}

func TestInvalidUpstreamCompletion(t *testing.T) {
	const invalidJSON = "Upstream returned invalid JSON or exceeded response limits."

	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{"malformed", "{", invalidJSON},
		{"deep", strings.Repeat("[", 200), invalidJSON},
		{"over the limit", strings.Repeat(" ", maxResponseBytes+1), invalidJSON},
		{"no choices", `{"choices":[]}`, "upstream must return exactly one completion choice"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			upstream := answerWith(http.StatusOK, nil, testCase.body)

			recorder := postMessages(newTestProxy(testConfig(), upstream), simpleRequest)
			assertAPIError(t, recorder, http.StatusBadGateway, "api_error", testCase.wantMessage)
		})
	}
}

func TestUpstreamBodyReadFailures(t *testing.T) {
	const readFailure = "Could not read the upstream response before the request deadline."

	t.Run("read error", func(t *testing.T) {
		upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
			response := upstreamReply(http.StatusOK, nil, "")
			response.Body = io.NopCloser(failingReader{})
			return response, nil
		}}

		recorder := postMessages(newTestProxy(testConfig(), upstream), simpleRequest)
		assertAPIError(t, recorder, http.StatusBadGateway, "api_error", readFailure)
	})

	t.Run("deadline", func(t *testing.T) {
		cfg := testConfig()
		cfg.requestTimeout = time.Millisecond
		upstream := &fakeUpstream{respond: func(request *http.Request) (*http.Response, error) {
			// A real transport ends the body read when the request context ends.
			reader, writer := io.Pipe()
			context.AfterFunc(request.Context(), func() {
				writer.CloseWithError(request.Context().Err())
			})
			response := upstreamReply(http.StatusOK, nil, "")
			response.Body = reader
			return response, nil
		}}

		recorder := postMessages(newTestProxy(cfg, upstream), simpleRequest)
		assertAPIError(t, recorder, http.StatusGatewayTimeout, "api_error", readFailure)
	})
}

func TestChatStreaming(t *testing.T) {
	application := newTestProxy(testConfig(), answerWithStream(chatStream()))
	recorder := postMessages(application, streamingRequest)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", recorder.Code, recorder.Body)
	}
	assertHeader(t, recorder.Header(), "Content-Type", "text/event-stream; charset=utf-8")
	assertHeader(t, recorder.Header(), "Cache-Control", "no-cache")
	assertHeader(t, recorder.Header(), "X-Accel-Buffering", "no")

	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	got := downstreamEventNames(recorder.Body.String())
	if !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events = %v, want %v", got, wantEvents)
	}
	if !strings.Contains(recorder.Body.String(), `"text":"Hello"`) {
		t.Errorf("body lacks the upstream text: %s", recorder.Body)
	}
}

func TestChatStreamIgnoresFramesWithoutContent(t *testing.T) {
	body := ": comment\n\n" + "event: ping\ndata: {}\n\n" + "data: \n\n" +
		sseBody(textChunk("Hello"), finishChunk("stop"))

	application := newTestProxy(testConfig(), answerWithStream(body))
	recorder := postMessages(application, streamingRequest)

	got := downstreamEventNames(recorder.Body.String())
	if got[len(got)-1] != "message_stop" {
		t.Errorf("events = %v, want a completed stream", got)
	}
}

func TestChatStreamStopsAtDoneMarker(t *testing.T) {
	body := chatStream() + "data: {invalid\n\n"

	application := newTestProxy(testConfig(), answerWithStream(body))
	recorder := postMessages(application, streamingRequest)

	if strings.Contains(recorder.Body.String(), "event: error") {
		t.Errorf("the proxy read data after [DONE]: %s", recorder.Body)
	}
}

func TestStreamFailuresBecomeErrorEvents(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"error event", "event: error\ndata: {}\n\n", "Upstream sent a streaming error."},
		{"invalid JSON", "data: {invalid\n\n", "Upstream sent an invalid JSON stream event."},
		{
			"deep JSON", "data: " + strings.Repeat("[", 200) + "\n\n",
			"Upstream sent an invalid JSON stream event.",
		},
		{
			"interrupted", sseBody(textChunk("cut")),
			"upstream stream ended before a finish reason",
		},
		{
			"done before the finish reason", sseBody(textChunk("cut")) + "data: [DONE]\n\n",
			"upstream stream ended before a finish reason",
		},
		{
			"translation failure", sseBody(`{"error":{"message":"private"}}`),
			"upstream sent a streaming error",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			application := newTestProxy(testConfig(), answerWithStream(testCase.body))
			recorder := postMessages(application, streamingRequest)

			body := recorder.Body.String()
			wantEvent := "event: error\ndata: " +
				marshal(t, messageEvent{Type: "error", Error: &apiError{
					Type: "api_error", Message: testCase.want,
				}}) + "\n\n"
			if !strings.HasSuffix(body, wantEvent) {
				t.Errorf("body = %q, want the suffix %q", body, wantEvent)
			}
			if strings.Contains(body, "message_stop") {
				t.Errorf("a failed stream reported completion: %s", body)
			}
		})
	}
}

func TestStreamContentType(t *testing.T) {
	const wrongType = "Upstream did not return a text/event-stream response."

	cases := []struct {
		name        string
		cfg         func() config
		contentType string
		isAccepted  bool
	}{
		{"chat event stream", testConfig, "text/event-stream", true},
		{"chat with parameters", testConfig, "text/event-stream; charset=utf-8", true},
		{"chat without a type", testConfig, "", false},
		{"chat plain text", testConfig, "text/plain", false},
		{"ChatGPT event stream", chatGPTTestConfig, "text/event-stream", true},
		{"ChatGPT without a type", chatGPTTestConfig, "", true},
		{"ChatGPT plain text", chatGPTTestConfig, "text/plain; charset=utf-8", true},
		{"ChatGPT JSON", chatGPTTestConfig, "application/json", false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := chatStream()
			cfg := testCase.cfg()
			if cfg.authMode == "chatgpt" {
				body = responsesStreamBody()
			}
			headers := map[string]string{}
			if testCase.contentType != "" {
				headers["Content-Type"] = testCase.contentType
			}
			upstream := answerWith(http.StatusOK, headers, body)

			recorder := postMessages(newTestProxy(cfg, upstream), streamingRequest)
			if !testCase.isAccepted {
				assertAPIError(t, recorder, http.StatusBadGateway, "api_error", wrongType)
				return
			}
			if !strings.Contains(recorder.Body.String(), "message_stop") {
				t.Errorf("the stream did not complete: %s", recorder.Body)
			}
		})
	}
}

func TestChatGPTStreaming(t *testing.T) {
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(responsesStreamBody()))
	recorder := postMessages(application, streamingRequest)

	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop", "message_delta", "message_stop",
	}
	got := downstreamEventNames(recorder.Body.String())
	if !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events = %v, want %v", got, wantEvents)
	}
}

func TestChatGPTBufferedReply(t *testing.T) {
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(responsesStreamBody()))
	recorder := postMessages(application, simpleRequest)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", recorder.Code, recorder.Body)
	}
	assertHeader(t, recorder.Header(), "Content-Type", "application/json")
	assertHeader(t, recorder.Header(), "X-Ant2oai-Usage", "")

	var message messageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertJSONEqual(t, marshal(t, message.Content), `[{"type":"text","text":"Hello world"}]`)
	assertJSONEqual(t, marshal(t, message.Usage), translatedTestUsage)
}

func TestChatGPTBufferedReplyWithoutUsage(t *testing.T) {
	body := namedSSEBody(responseCreated, responseCompleted(`[]`))
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(body))

	recorder := postMessages(application, simpleRequest)
	assertHeader(t, recorder.Header(), "X-Ant2oai-Usage", "unavailable")
}

func TestChatGPTBufferedReplyAppliesStopSequences(t *testing.T) {
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(responsesStreamBody()))

	recorder := postMessages(application, requestWith(`"stop_sequences":["world"],`+userMessages))
	var message messageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertJSONEqual(t, marshal(t, message.Content), `[{"type":"text","text":"Hello "}]`)
	if message.StopSequence == nil || *message.StopSequence != "world" {
		t.Errorf("stop_sequence = %v, want world", message.StopSequence)
	}
}

func TestChatGPTErrorEventKeepsItsCode(t *testing.T) {
	// Only the Chat Completions route treats the event name as a generic failure.
	body := "event: error\ndata: " +
		`{"type":"error","code":"subscription_sharing_invalid_user"}` + "\n\n"
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(body))

	recorder := postMessages(application, streamingRequest)
	if !strings.Contains(recorder.Body.String(), "subscription_sharing_invalid_user") {
		t.Errorf("body lacks the known failure code: %s", recorder.Body)
	}
}

func TestChatGPTBufferedStreamFailure(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			"interrupted", namedSSEBody(responseCreated),
			"upstream Responses stream ended before response completion",
		},
		{
			"done marker before completion", namedSSEBody(responseCreated) + "data: [DONE]\n\n",
			"upstream Responses stream ended before response completion",
		},
		{"invalid event", "data: {invalid\n\n", "upstream sent an invalid Responses stream event"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			application := newTestProxy(chatGPTTestConfig(), answerWithStream(testCase.body))

			recorder := postMessages(application, simpleRequest)
			assertAPIError(t, recorder, http.StatusBadGateway, "api_error", testCase.wantMessage)
		})
	}
}

func TestChatGPTStreamStopsAtTerminalEvent(t *testing.T) {
	// Later data is never read, so its invalid JSON cannot fail the reply.
	body := responsesStreamBody() + "data: {invalid\n\n"
	application := newTestProxy(chatGPTTestConfig(), answerWithStream(body))

	recorder := postMessages(application, simpleRequest)
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d; body: %s", recorder.Code, recorder.Body)
	}
}

func TestStreamTimeouts(t *testing.T) {
	const chatPrefix = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"
	const idleMessage = "Upstream stream exceeded the idle timeout."
	const deadlineMessage = "Upstream request timed out."

	cases := []struct {
		name        string
		cfg         func() config
		isIdle      bool
		prefix      string
		body        string
		isBuffered  bool
		wantStatus  int
		wantMessage string
	}{
		{
			name: "idle stream", cfg: testConfig, isIdle: true, prefix: chatPrefix,
			body: streamingRequest, wantMessage: idleMessage,
		},
		{
			name: "stream deadline", cfg: testConfig, prefix: chatPrefix,
			body: streamingRequest, wantMessage: deadlineMessage,
		},
		{
			name: "idle buffered reply", cfg: chatGPTTestConfig, isIdle: true,
			body: simpleRequest, isBuffered: true, wantStatus: http.StatusBadGateway,
			wantMessage: idleMessage,
		},
		{
			name: "buffered reply deadline", cfg: chatGPTTestConfig,
			body: simpleRequest, isBuffered: true, wantStatus: http.StatusGatewayTimeout,
			wantMessage: deadlineMessage,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testCase.cfg()
			deadline := newManualDeadline()
			if testCase.isIdle {
				cfg.upstreamIdle = time.Millisecond
			}
			upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
				// The stream has started, so the deadline can now interrupt it.
				if !testCase.isIdle {
					deadline.expire()
				}
				return stalledStream(testCase.prefix), nil
			}}

			recorder := serve(newTestProxy(cfg, upstream), newMessagesRequest(deadline, testCase.body))
			if testCase.isBuffered {
				assertAPIError(t, recorder, testCase.wantStatus, "api_error", testCase.wantMessage)
				return
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "event: error") || !strings.Contains(body, testCase.wantMessage) {
				t.Errorf("body = %q, want an error event with %q", body, testCase.wantMessage)
			}
		})
	}
}

func TestClientDisconnectStopsStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
		// The reply has already started when the client goes away.
		defer cancel()
		return stalledStream(""), nil
	}}

	recorder := serve(newTestProxy(testConfig(), upstream),
		newMessagesRequest(ctx, streamingRequest))
	if strings.Contains(recorder.Body.String(), "event: error") {
		t.Errorf("wrote an error to a disconnected client: %s", recorder.Body)
	}
}

// trackedBody reports whether the proxy closed the upstream response body.
type trackedBody struct {
	io.Reader
	isClosed atomic.Bool
}

func (body *trackedBody) Close() error {
	body.isClosed.Store(true)
	return nil
}

// brokenClient is a response writer whose client stops reading after a number
// of successful writes.
type brokenClient struct {
	header          http.Header
	remainingWrites int
}

func (client *brokenClient) Header() http.Header { return client.header }
func (client *brokenClient) WriteHeader(int)     {}
func (client *brokenClient) Flush()              {}

func (client *brokenClient) Write(data []byte) (int, error) {
	if client.remainingWrites == 0 {
		return 0, errors.New("client disconnected")
	}
	client.remainingWrites--
	return len(data), nil
}

func TestDownstreamWriteFailureClosesUpstream(t *testing.T) {
	// The stream produces six events, so each count fails at a different stage.
	for writes := 0; writes < 6; writes++ {
		body := &trackedBody{Reader: strings.NewReader(chatStream())}
		upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
			response := upstreamReply(http.StatusOK,
				map[string]string{"Content-Type": "text/event-stream"}, "")
			response.Body = body
			return response, nil
		}}

		client := &brokenClient{header: http.Header{}, remainingWrites: writes}
		newTestProxy(testConfig(), upstream).ServeHTTP(client,
			newMessagesRequest(context.Background(), streamingRequest))

		if !body.isClosed.Load() {
			t.Errorf("after %d writes: the upstream body stayed open", writes)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	cfg := testConfig()
	cfg.maxConcurrent = 1

	entered := make(chan struct{})
	release := make(chan struct{})
	var isFirstCall atomic.Bool
	isFirstCall.Store(true)
	upstream := &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
		if isFirstCall.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return upstreamReply(http.StatusOK, nil, simpleCompletion), nil
	}}
	application := newTestProxy(cfg, upstream)

	firstStatus := make(chan int, 1)
	go func() { firstStatus <- postMessages(application, simpleRequest).Code }()
	<-entered

	rejected := postMessages(application, simpleRequest)
	assertAPIError(t, rejected, http.StatusTooManyRequests, "rate_limit_error",
		"Proxy request or concurrency limit reached.")
	assertHeader(t, rejected.Header(), "Retry-After", "1")

	close(release)
	if status := <-firstStatus; status != http.StatusOK {
		t.Errorf("first request status = %d", status)
	}

	// A finished request returns its slot.
	if recorder := postMessages(application, simpleRequest); recorder.Code != http.StatusOK {
		t.Errorf("status after release = %d", recorder.Code)
	}
}

func TestRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.maxConcurrent = 2
	cfg.requestsPerMinute = 1
	application := newTestProxy(cfg, answerWith(http.StatusOK, nil, simpleCompletion))

	// The burst size equals max-concurrent.
	for attempt := 0; attempt < 2; attempt++ {
		if recorder := postMessages(application, simpleRequest); recorder.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d", attempt, recorder.Code)
		}
	}

	recorder := postMessages(application, simpleRequest)
	assertAPIError(t, recorder, http.StatusTooManyRequests, "rate_limit_error",
		"Proxy request or concurrency limit reached.")
}

func TestRateLimitRejectionReturnsItsSlot(t *testing.T) {
	cfg := testConfig()
	cfg.maxConcurrent = 1
	cfg.requestsPerMinute = 1
	application := newTestProxy(cfg, answerWith(http.StatusOK, nil, simpleCompletion))

	postMessages(application, simpleRequest)
	for attempt := 0; attempt < 3; attempt++ {
		postMessages(application, simpleRequest)
	}

	// A lost slot looks like a rate rejection until the token bucket refills, so
	// only the slot count can show this failure without waiting for the clock.
	if len(application.slots) != 0 {
		t.Errorf("%d slots stayed occupied after rejected requests", len(application.slots))
	}
}

func TestConcurrentRequests(t *testing.T) {
	cfg := testConfig()
	cfg.maxConcurrent = 32
	application := newTestProxy(cfg, answerWith(http.StatusOK, nil, simpleCompletion))

	var group sync.WaitGroup
	statuses := make([]int, cfg.maxConcurrent)
	for index := range statuses {
		group.Add(1)
		go func() {
			defer group.Done()
			statuses[index] = postMessages(application, simpleRequest).Code
		}()
	}
	group.Wait()

	for index, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d status = %d", index, status)
		}
	}
}

func TestUpstreamClientRefusesRedirects(t *testing.T) {
	client := newUpstreamClient(testConfig())

	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect error = %v, want %v", err, http.ErrUseLastResponse)
	}
}

// captureLogs replaces the process logger for one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func TestCompletionLogRecordsFailureSource(t *testing.T) {
	t.Run("proxy failure", func(t *testing.T) {
		logs := captureLogs(t)
		postMessages(newTestProxy(testConfig(), &fakeUpstream{}), `{`)

		for _, want := range []string{
			"request completed", "status=400", "error_source=proxy",
			"error_type=invalid_request_error",
		} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("log lacks %q: %s", want, logs)
			}
		}
	})

	t.Run("upstream failure", func(t *testing.T) {
		logs := captureLogs(t)
		postMessages(newTestProxy(testConfig(), answerWith(500, nil, "")), simpleRequest)

		for _, want := range []string{"status=502", "error_source=upstream", "upstream_status=500"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("log lacks %q: %s", want, logs)
			}
		}
	})

	t.Run("stream failure after the headers", func(t *testing.T) {
		logs := captureLogs(t)
		application := newTestProxy(testConfig(), answerWithStream(sseBody(textChunk("cut"))))
		postMessages(application, streamingRequest)

		for _, want := range []string{"stream failed", "status=200", "error_type=api_error"} {
			if !strings.Contains(logs.String(), want) {
				t.Errorf("log lacks %q: %s", want, logs)
			}
		}
	})
}

func TestSecretsStayOutOfResponsesAndLogs(t *testing.T) {
	const upstreamBody = `{"error":{"code":"other","message":"leaked-provider-text"}}`
	secrets := []string{testUpstreamKey, testProxyKey, "leaked-provider-text", "prompt-text"}
	request := requestWith(`"messages":[{"role":"user","content":"prompt-text"}]`)

	cases := []struct {
		name string
		cfg  func() config
	}{
		{"chat", testConfig},
		{"ChatGPT", chatGPTTestConfig},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			logs := captureLogs(t)
			cfg := testCase.cfg()
			cfg.proxyAPIKey = testProxyKey
			upstream := answerWith(http.StatusBadRequest, nil, upstreamBody)

			inbound := newMessagesRequest(context.Background(), request)
			inbound.Header.Set("X-Api-Key", testProxyKey)
			recorder := serve(newTestProxy(cfg, upstream), inbound)

			for _, secret := range secrets {
				if strings.Contains(recorder.Body.String(), secret) {
					t.Errorf("response contains %q: %s", secret, recorder.Body)
				}
				if strings.Contains(logs.String(), secret) {
					t.Errorf("log contains %q: %s", secret, logs)
				}
			}
		})
	}
}
