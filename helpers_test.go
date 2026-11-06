package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testUpstreamKey = "upstream-secret"
	testHostID      = "urn:uuid:11111111-2222-4333-8444-555555555555"

	// simpleRequest is the smallest valid Messages request.
	simpleRequest = `{"model":"claude","max_tokens":10,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	streamingRequest = `{"model":"claude","max_tokens":10,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`

	// simpleCompletion is a complete Chat Completions reply to simpleRequest.
	simpleCompletion = `{"choices":[{"index":0,"finish_reason":"stop",` +
		`"message":{"role":"assistant","content":"hello"}}]}`
)

// testHome replaces the home directory for the whole test process.
var testHome string

// TestMain isolates every test from the developer's machine. The default
// credential path depends on the home directory, so a test or a bug in the code
// under test can never reach real tokens, and no real API key is visible.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "ant2oai-test-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test home directory:", err)
		return 1
	}
	defer os.RemoveAll(home)
	testHome = home

	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		if err := os.Setenv(name, home); err != nil {
			fmt.Fprintln(os.Stderr, "isolate test home directory:", err)
			return 1
		}
	}
	for _, name := range configEnvironment {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, "clear test environment:", err)
			return 1
		}
	}
	return m.Run()
}

// Fixed dates keep expiry checks independent of the time when the tests run.
var (
	farFuture = time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	longPast  = time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// fakeUpstream replaces the network transport. Tests choose every upstream reply
// and can inspect every request that the proxy sends.
type fakeUpstream struct {
	respond func(request *http.Request) (*http.Response, error)

	mutex sync.Mutex
	calls []upstreamCall
}

type upstreamCall struct {
	method string
	url    string
	header http.Header
	body   string
}

func (fake *fakeUpstream) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}

	fake.mutex.Lock()
	fake.calls = append(fake.calls, upstreamCall{
		method: request.Method, url: request.URL.String(),
		header: request.Header.Clone(), body: string(body),
	})
	fake.mutex.Unlock()

	return fake.respond(request)
}

func (fake *fakeUpstream) recorded() []upstreamCall {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]upstreamCall(nil), fake.calls...)
}

// upstreamReply builds a response with the given status, headers, and body.
func upstreamReply(status int, headers map[string]string, body string) *http.Response {
	header := http.Header{}
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{
		StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)),
	}
}

// answerWith returns a fake upstream that gives the same reply to every request.
func answerWith(status int, headers map[string]string, body string) *fakeUpstream {
	return &fakeUpstream{respond: func(*http.Request) (*http.Response, error) {
		return upstreamReply(status, headers, body), nil
	}}
}

func answerWithStream(body string) *fakeUpstream {
	return answerWith(http.StatusOK, map[string]string{"Content-Type": "text/event-stream"}, body)
}

// stalledStream sends prefix and then delivers nothing more. Its body never
// reports an error, so only the timers and context of the proxy can end the
// request. That keeps the outcome independent of goroutine scheduling.
func stalledStream(prefix string) *http.Response {
	reader, writer := io.Pipe()
	if prefix != "" {
		// The write fails when the proxy closes the body, which ends this goroutine.
		go writer.Write([]byte(prefix))
	}

	response := upstreamReply(http.StatusOK,
		map[string]string{"Content-Type": "text/event-stream"}, "")
	response.Body = reader
	return response
}

// manualDeadline is a request context whose deadline the test triggers at an
// exact point. A real timer could expire before the code under test is ready.
type manualDeadline struct {
	context.Context
	expired chan struct{}
	once    sync.Once
}

func newManualDeadline() *manualDeadline {
	return &manualDeadline{Context: context.Background(), expired: make(chan struct{})}
}

func (deadline *manualDeadline) Done() <-chan struct{} { return deadline.expired }

func (deadline *manualDeadline) Err() error {
	select {
	case <-deadline.expired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (deadline *manualDeadline) expire() {
	deadline.once.Do(func() { close(deadline.expired) })
}

func testConfig() config {
	return config{
		authMode: "api-key", account: "default", upstreamBaseURL: "https://upstream.test/v1",
		upstreamAPIKey: testUpstreamKey, modelMap: map[string]string{},
		tokenLimitField: "max_tokens", streamUsage: true, requestTimeout: time.Minute,
		upstreamIdle: time.Minute, maxBodyBytes: 1 << 20, maxConcurrent: 8,
		requestsPerMinute: 1 << 30,
	}
}

func chatGPTTestConfig() config {
	cfg := testConfig()
	cfg.authMode = "chatgpt"
	cfg.upstreamBaseURL = chatGPTResource
	return cfg
}

func testCredentials() oauthCredentials {
	return oauthCredentials{
		Issuer: chatGPTIssuer, ClientID: "app_client", Subject: "subject-1",
		Email: "person@example.com", AccessToken: "access-1", RefreshToken: "refresh-1",
		IDToken: "id-1", ExpiresAt: farFuture,
		Scopes: []string{"openid", "offline_access", "resource.invoke", chatGPTScope},
	}
}

func testStore(credentials oauthCredentials) oauthStore {
	return oauthStore{
		HostID: testHostID, Accounts: map[string]oauthCredentials{"default": credentials},
	}
}

// credentialDirectory returns a private directory. The credential lock rejects
// directories that other users can write, and t.TempDir follows the user's umask.
func credentialDirectory(t *testing.T) string {
	t.Helper()

	directory := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatalf("create credential directory: %v", err)
	}
	return directory
}

// newTestProxy returns a proxy that sends all upstream traffic to the fake. In
// ChatGPT mode it also has a session with a valid token that does not expire.
func newTestProxy(cfg config, upstream *fakeUpstream) *proxy {
	application := newProxy(cfg)
	application.client.Transport = upstream
	if cfg.authMode == "chatgpt" {
		application.oauth = &oauthSession{
			path: "/nonexistent/chatgpt.json", account: "default",
			store: testStore(testCredentials()),
		}
	}
	return application
}

func serve(application *proxy, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	application.ServeHTTP(recorder, request)
	return recorder
}

func newMessagesRequest(ctx context.Context, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(ctx)
}

func postMessages(application *proxy, body string) *httptest.ResponseRecorder {
	return serve(application, newMessagesRequest(context.Background(), body))
}

// assertAPIError checks the status, type, and message of an error response.
func assertAPIError(
	t *testing.T, recorder *httptest.ResponseRecorder, status int, kind, message string,
) {
	t.Helper()

	if recorder.Code != status {
		t.Errorf("status = %d, want %d; body: %s", recorder.Code, status, recorder.Body)
	}
	var response errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode error response %q: %v", recorder.Body, err)
	}
	if response.Type != "error" || response.Error.Type != kind {
		t.Errorf("error type = %q/%q, want error/%q", response.Type, response.Error.Type, kind)
	}
	if response.Error.Message != message {
		t.Errorf("message = %q, want %q", response.Error.Message, message)
	}
	if !strings.HasPrefix(response.RequestID, "req_") {
		t.Errorf("request_id = %q, want a req_ prefix", response.RequestID)
	}
}

func assertHeader(t *testing.T, header http.Header, name, want string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Errorf("header %s = %q, want %q", name, got, want)
	}
}

func assertError(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("error = nil, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func marshal(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return string(data)
}

// assertJSONEqual compares two JSON documents without regard to whitespace or
// object key order.
func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("decode actual JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode expected JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("JSON differs\n got: %s\nwant: %s", compactJSON(got), compactJSON(want))
	}
}

func assertEvents(t *testing.T, got []messageEvent, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d\n got: %s", len(got), len(want), marshal(t, got))
	}
	for index, event := range got {
		assertJSONEqual(t, marshal(t, event), want[index])
	}
}

// compactJSON removes the whitespace from a fixture, so that a fixture written
// on several lines can become one SSE data line.
func compactJSON(source string) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, []byte(source)); err != nil {
		panic("invalid JSON fixture: " + err.Error())
	}
	return buffer.String()
}

// sseBody renders JSON fixtures as unnamed SSE events.
func sseBody(frames ...string) string {
	var body strings.Builder
	for _, frame := range frames {
		fmt.Fprintf(&body, "data: %s\n\n", compactJSON(frame))
	}
	return body.String()
}

// namedSSEBody renders JSON fixtures as SSE events named after their type field.
func namedSSEBody(frames ...string) string {
	var body strings.Builder
	for _, frame := range frames {
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(frame), &event); err != nil {
			panic("invalid JSON fixture: " + err.Error())
		}
		fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", event.Type, compactJSON(frame))
	}
	return body.String()
}

// downstreamEventNames returns the event names of an SSE body in order.
func downstreamEventNames(body string) []string {
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if name, isEvent := strings.CutPrefix(line, "event: "); isEvent {
			names = append(names, name)
		}
	}
	return names
}
