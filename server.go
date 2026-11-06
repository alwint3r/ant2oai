package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	downstreamWriteTimeout = 30 * time.Second
	streamPingInterval     = 15 * time.Second
	maxUpstreamErrorBytes  = 64 << 10
	maxRetryAfterSeconds   = 24 * 60 * 60
)

type proxy struct {
	config config
	client *http.Client
	oauth  *oauthSession
	slots  chan struct{}

	rateMutex     sync.Mutex
	rateTokens    float64
	rateUpdatedAt time.Time
}

func newUpstreamClient(cfg config) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = dialer.DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = cfg.upstreamIdle
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConns = cfg.maxConcurrent
	transport.MaxIdleConnsPerHost = cfg.maxConcurrent
	transport.MaxResponseHeaderBytes = 32 << 10

	return &http.Client{
		Transport: transport,
		// Refuse every redirect so API credentials stay at the configured URL.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newProxy(cfg config) *proxy {
	return &proxy{
		config:     cfg,
		client:     newUpstreamClient(cfg),
		slots:      make(chan struct{}, cfg.maxConcurrent),
		rateTokens: float64(cfg.maxConcurrent), rateUpdatedAt: time.Now(),
	}
}

// ServeHTTP runs one request through its stages in order. A stage helper that
// returns false has already written the error response. The deferred cleanups
// stay here, so that the lifetime of each resource is visible in one place.
func (server *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Probes run before request IDs, logging, and authentication.
	isHealthMethod := request.Method == http.MethodGet || request.Method == http.MethodHead
	if request.URL.Path == "/healthz" && isHealthMethod {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.URL.Path == "/api/hello" && request.Method == http.MethodHead {
		writer.WriteHeader(http.StatusNoContent)
		return
	}

	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Could not allocate a request ID.", "")
		return
	}
	idSuffix := hex.EncodeToString(randomID[:])
	requestID := "req_" + idSuffix
	writer.Header().Set("request-id", requestID)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	recorded := &statusWriter{ResponseWriter: writer}
	defer logRequestCompletion(recorded, requestID, time.Now())
	writer = recorded

	// Admission.
	if !server.acceptRoute(writer, request, requestID) {
		return
	}
	if !server.admit() {
		writer.Header().Set("Retry-After", "1")
		writeAPIError(writer, http.StatusTooManyRequests, "rate_limit_error",
			"Proxy request or concurrency limit reached.", requestID)
		return
	}
	defer func() { <-server.slots }()

	// Translation.
	input, ok := server.readMessagesRequest(writer, request, requestID)
	if !ok {
		return
	}
	payload, ok := server.encodeUpstreamPayload(writer, input, requestID)
	if !ok {
		return
	}

	// Upstream exchange.
	ctx, cancel := context.WithTimeout(request.Context(), server.config.requestTimeout)
	defer cancel()
	upstream, ok := server.newUpstreamRequest(ctx, writer, payload, input.Stream, requestID)
	if !ok {
		return
	}

	// Inbound credentials, beta headers, cookies, and tracing metadata stay local.
	response, err := server.client.Do(upstream)
	if err != nil {
		// A disconnected client cannot receive an error response.
		if request.Context().Err() == nil {
			writeTransportError(writer, ctx, err, requestID)
		}
		return
	}
	defer response.Body.Close()

	isChatGPT := server.config.usesChatGPT()
	if isChatGPT {
		if upstreamID := response.Header.Get("X-Request-Id"); safeUpstreamRequestID(upstreamID) {
			writer.Header().Set("X-Ant2oai-Upstream-Request-Id", upstreamID)
		}
	}
	if response.StatusCode != http.StatusOK {
		server.writeUpstreamError(writer, response, requestID)
		return
	}

	// Reply.
	messageID := "msg_" + idSuffix
	if input.Stream || isChatGPT {
		if !isEventStream(response, isChatGPT) {
			writeAPIError(writer, http.StatusBadGateway, "api_error",
				"Upstream did not return a text/event-stream response.", requestID)
			return
		}
		server.serveStream(writer, ctx, response.Body, input, messageID, requestID)
		return
	}
	writeCompletion(writer, ctx, response.Body, input, messageID, requestID)
}

// logRequestCompletion runs when a request ends. It records only proxy-controlled
// failure text, because writeAPIError receives no raw provider messages.
func logRequestCompletion(recorded *statusWriter, requestID string, started time.Time) {
	fields := []any{
		"request_id", requestID, "status", recorded.status,
		"duration", time.Since(started).Round(time.Millisecond),
	}
	if recorded.failure != nil {
		source := "proxy"
		upstreamStatus := recorded.Header().Get("X-Ant2oai-Upstream-Status")
		if upstreamStatus != "" {
			source = "upstream"
			fields = append(fields, "upstream_status", upstreamStatus)
		}
		fields = append(fields, "error_source", source,
			"error_type", recorded.failure.Type, "reason", recorded.failure.Message)
	}
	slog.Info("request completed", fields...)
}

// acceptRoute checks the credential, path, method, and content type. It reports
// false after it has written the error response.
func (server *proxy) acceptRoute(
	writer http.ResponseWriter, request *http.Request, requestID string,
) bool {
	if !server.isAuthorized(request) {
		writeAPIError(writer, http.StatusUnauthorized, "authentication_error",
			"Invalid proxy credential; set ANTHROPIC_AUTH_TOKEN to the configured proxy key.",
			requestID)
		return false
	}
	if request.URL.Path == "/v1/messages/count_tokens" {
		writeAPIError(writer, http.StatusNotFound, "not_found_error",
			"Exact token counting is unavailable for this upstream. "+
				"Claude Code can use its local estimate.", requestID)
		return false
	}
	if request.URL.Path != "/v1/messages" {
		writeAPIError(writer, http.StatusNotFound, "not_found_error",
			"Unknown endpoint. Use POST /v1/messages.", requestID)
		return false
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "invalid_request_error",
			"This endpoint requires POST.", requestID)
		return false
	}

	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json.", requestID)
		return false
	}
	return true
}

// readMessagesRequest reads and decodes the request body. It reports false after
// it has written the error response.
func (server *proxy) readMessagesRequest(
	writer http.ResponseWriter, request *http.Request, requestID string,
) (messagesRequest, bool) {
	var input messagesRequest

	raw, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, server.config.maxBodyBytes))
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, "request_too_large",
				"Request body exceeds max-body-bytes.", requestID)
		} else {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
				"Could not read the request body.", requestID)
		}
		return input, false
	}
	if err := checkJSONDepth(raw); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
			err.Error(), requestID)
		return input, false
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&input)
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		message := "Expected one valid Messages request with supported fields. " +
			"Disable experimental betas and check the compatibility guide in README.md."
		// Match this known field without exposing arbitrary decoder errors, which
		// can include client-supplied field names or values.
		if err != nil && err.Error() == `json: unknown field "safeguards"` {
			message = "safeguards: server-side auto-mode safety checks are unsupported. " +
				"Set CLAUDE_CODE_AUTO_MODE_SERVER=0 and " +
				"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1, then start a new session."
		}
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", message, requestID)
		return input, false
	}
	return input, true
}

// encodeUpstreamPayload translates the request to the upstream wire format. It
// reports false after it has written the error response.
func (server *proxy) encodeUpstreamPayload(
	writer http.ResponseWriter, input messagesRequest, requestID string,
) ([]byte, bool) {
	translated, err := translateRequest(input, server.config)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
			err.Error(), requestID)
		return nil, false
	}

	var upstreamInput any = translated
	if server.config.usesChatGPT() {
		converted, err := translateResponsesRequest(translated)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
				err.Error(), requestID)
			return nil, false
		}
		upstreamInput = converted
	}

	payload, err := json.Marshal(upstreamInput)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Could not encode the translated request.", requestID)
		return nil, false
	}
	return payload, true
}

// newUpstreamRequest selects the route and credential. OAuth may renew and persist
// credentials here, before inference starts. It reports false after it has
// written the error response.
func (server *proxy) newUpstreamRequest(
	ctx context.Context, writer http.ResponseWriter, payload []byte, isStreaming bool,
	requestID string,
) (*http.Request, bool) {
	isChatGPT := server.config.usesChatGPT()
	upstreamURL := server.config.upstreamBaseURL + "/chat/completions"
	credential := server.config.upstreamAPIKey
	if isChatGPT {
		if server.oauth == nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "api_error",
				"ChatGPT credentials are unavailable; stop the proxy and run ant2oai login.",
				requestID)
			return nil, false
		}
		token, err := server.oauth.accessToken(ctx)
		if err != nil {
			writer.Header().Set("X-Should-Retry", "false")
			writeAPIError(writer, http.StatusServiceUnavailable, "api_error",
				err.Error(), requestID)
			return nil, false
		}
		credential = token
		upstreamURL = chatGPTResource + "/responses"
	}

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL,
		bytes.NewReader(payload))
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Could not create the upstream request.", requestID)
		return nil, false
	}

	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	if isStreaming || isChatGPT {
		upstream.Header.Set("Accept", "text/event-stream")
	}
	if credential != "" {
		upstream.Header.Set("Authorization", "Bearer "+credential)
	}
	return upstream, true
}

func writeTransportError(
	writer http.ResponseWriter, ctx context.Context, err error, requestID string,
) {
	var networkError net.Error
	isTimeout := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if errors.As(err, &networkError) && networkError.Timeout() {
		isTimeout = true
	}

	if isTimeout {
		writeAPIError(writer, http.StatusGatewayTimeout, "api_error",
			"Upstream request timed out.", requestID)
		return
	}
	writeAPIError(writer, http.StatusBadGateway, "api_error",
		"Could not connect to the upstream. "+
			"Check the base URL, network, and TLS configuration.", requestID)
}

func isEventStream(response *http.Response, isChatGPT bool) bool {
	reportedContentType := response.Header.Get("Content-Type")
	contentType, _, _ := mime.ParseMediaType(reportedContentType)

	// The direct ChatGPT route can omit Content-Type. Accept its SSE with a
	// missing or text/plain type; the bounded parser still validates completion.
	if isChatGPT && (reportedContentType == "" || contentType == "text/plain") {
		return true
	}
	return contentType == "text/event-stream"
}

// writeCompletion translates one buffered Chat Completions reply.
func writeCompletion(
	writer http.ResponseWriter, ctx context.Context, body io.Reader,
	input messagesRequest, messageID, requestID string,
) {
	rawResponse, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		status := http.StatusBadGateway
		if ctx.Err() == context.DeadlineExceeded {
			status = http.StatusGatewayTimeout
		}
		writeAPIError(writer, status, "api_error",
			"Could not read the upstream response before the request deadline.", requestID)
		return
	}

	var completion completionResponse
	if len(rawResponse) > maxResponseBytes || checkJSONDepth(rawResponse) != nil ||
		json.Unmarshal(rawResponse, &completion) != nil {
		writeAPIError(writer, http.StatusBadGateway, "api_error",
			"Upstream returned invalid JSON or exceeded response limits.", requestID)
		return
	}

	output, err := translateResponse(completion, input, messageID)
	if err != nil {
		writeAPIError(writer, http.StatusBadGateway, "api_error", err.Error(), requestID)
		return
	}
	if completion.Usage == nil {
		writer.Header().Set("X-Ant2oai-Usage", "unavailable")
	}
	writeJSON(writer, http.StatusOK, output)
}

func (server *proxy) isAuthorized(request *http.Request) bool {
	if server.config.proxyAPIKey == "" {
		return true
	}

	var bearer string
	scheme, value, found := strings.Cut(request.Header.Get("Authorization"), " ")
	if found && strings.EqualFold(scheme, "Bearer") {
		bearer = value
	}

	// Compare digests in constant time, and always check both headers.
	expected := sha256.Sum256([]byte(server.config.proxyAPIKey))
	providedAPIKey := sha256.Sum256([]byte(request.Header.Get("X-Api-Key")))
	providedBearer := sha256.Sum256([]byte(bearer))
	apiKeyMatches := subtle.ConstantTimeCompare(expected[:], providedAPIKey[:])
	bearerMatches := subtle.ConstantTimeCompare(expected[:], providedBearer[:])
	return apiKeyMatches|bearerMatches == 1
}

func (server *proxy) admit() bool {
	select {
	case server.slots <- struct{}{}:
	default:
		return false
	}

	// Token bucket: refill for the elapsed time, then take one token.
	server.rateMutex.Lock()
	now := time.Now()
	elapsedMinutes := now.Sub(server.rateUpdatedAt).Minutes()
	refilledTokens := server.rateTokens + elapsedMinutes*float64(server.config.requestsPerMinute)
	server.rateTokens = math.Min(float64(server.config.maxConcurrent), refilledTokens)
	server.rateUpdatedAt = now
	isAllowed := server.rateTokens >= 1
	if isAllowed {
		server.rateTokens--
	}
	server.rateMutex.Unlock()

	if !isAllowed {
		<-server.slots
	}
	return isAllowed
}

func (server *proxy) writeUpstreamError(
	writer http.ResponseWriter, response *http.Response, requestID string,
) {
	if server.config.usesChatGPT() {
		writeChatGPTUpstreamError(writer, response, requestID)
		return
	}

	writer.Header().Set("X-Ant2oai-Upstream-Status", strconv.Itoa(response.StatusCode))
	status := http.StatusBadGateway
	errorType := "api_error"
	message := fmt.Sprintf("Upstream returned HTTP %d. Check its configuration and availability.",
		response.StatusCode)
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		status = http.StatusBadRequest
		errorType = "invalid_request_error"
		message = "Upstream rejected the translated request. " +
			"Check token limits, sampling parameters, tool schemas, and model support."
		if hasContextLengthCode(response.Body) {
			message = "Prompt is too long: upstream context window exceeded. " +
				"Compact the conversation or select a larger-context model."
		}

	case http.StatusUnauthorized, http.StatusForbidden:
		message = "Upstream authentication or permission check failed. " +
			"Check OPENAI_API_KEY and model access."
		writer.Header().Set("X-Should-Retry", "false")

	case http.StatusNotFound:
		status = http.StatusBadRequest
		errorType = "invalid_request_error"
		message = "Upstream endpoint or model was not found. " +
			"Check OPENAI_BASE_URL (including its API prefix) and model mapping."

	case http.StatusTooManyRequests:
		status = http.StatusTooManyRequests
		errorType = "rate_limit_error"
		message = "Upstream rate limit reached."

	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		status = http.StatusGatewayTimeout
		message = "Upstream request timed out."
	}

	copyRetryHeaders(writer, response)
	writeAPIError(writer, status, errorType, message, requestID)
}

// hasContextLengthCode matches only a known code. Provider messages can contain
// credentials or prompt fragments, so they are never copied to responses or logs.
func hasContextLengthCode(body io.Reader) bool {
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxUpstreamErrorBytes))
	if err != nil || json.Unmarshal(raw, &failure) != nil {
		return false
	}
	return failure.Error.Code == "context_length_exceeded"
}

func copyRetryHeaders(writer http.ResponseWriter, response *http.Response) {
	isAuthFailure := response.StatusCode == http.StatusUnauthorized ||
		response.StatusCode == http.StatusForbidden
	if !isAuthFailure {
		if retry := response.Header.Get("X-Should-Retry"); retry == "true" || retry == "false" {
			writer.Header().Set("X-Should-Retry", retry)
		}
	}

	if seconds, ok := retryAfterSeconds(response.Header.Get("Retry-After")); ok {
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
}

// retryAfterSeconds accepts a delay in seconds or an HTTP date. It reports false
// for a value that is missing, malformed, in the past, or too far ahead.
func retryAfterSeconds(value string) (int, bool) {
	if value == "" {
		return 0, false
	}

	seconds, err := strconv.Atoi(value)
	if err != nil {
		retryAt, parseError := http.ParseTime(value)
		if parseError != nil {
			return 0, false
		}
		seconds = int(math.Ceil(time.Until(retryAt).Seconds()))
	}
	return seconds, seconds >= 0 && seconds <= maxRetryAfterSeconds
}

func (server *proxy) serveStream(
	writer http.ResponseWriter, ctx context.Context, body io.ReadCloser,
	input messagesRequest, messageID, requestID string,
) {
	// ChatGPT always streams upstream. A non-streaming Messages client receives
	// one JSON response after the same stream validation has completed.
	if input.Stream {
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("X-Accel-Buffering", "no")
		message := newMessage(input.Model, messageID)
		start := messageEvent{Type: "message_start", Message: &message}
		if err := writeEvent(writer, start); err != nil {
			return
		}
	}

	// The reader must end before this function returns, so that it cannot use
	// the response body after the HTTP layer has released it.
	readContext, cancelReader := context.WithCancel(ctx)
	frames := make(chan sseFrame, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		readSSE(readContext, body, frames)
	}()
	defer func() {
		cancelReader()
		body.Close()
		<-readerDone
	}()

	pings := time.NewTicker(streamPingInterval)
	defer pings.Stop()
	idle := time.NewTimer(server.config.upstreamIdle)
	defer idle.Stop()

	translator := newStreamTranslator(input.Tools)
	var responses *responsesStream
	if server.config.usesChatGPT() {
		responses = newResponsesStream(input.Tools)
	}

	streamError := func(message string) {
		slog.Warn("stream failed", "request_id", requestID, "reason", message)
		failure := &apiError{Type: "api_error", Message: message}
		if recorded, ok := writer.(*statusWriter); ok {
			recorded.failure = failure
		}
		if input.Stream {
			writeEvent(writer, messageEvent{Type: "error", Error: failure})
		} else {
			status := http.StatusBadGateway
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			writeAPIError(writer, status, "api_error", message, requestID)
		}
	}

	isComplete := false
	for !isComplete {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				streamError("Upstream request timed out.")
			}
			return
		case <-idle.C:
			streamError("Upstream stream exceeded the idle timeout.")
			return
		case <-pings.C:
			if input.Stream {
				if err := writeEvent(writer, messageEvent{Type: "ping"}); err != nil {
					return
				}
			}

		case frame, isOpen := <-frames:
			if !isOpen {
				isComplete = true
				continue
			}
			if frame.err != nil {
				streamError(frame.err.Error())
				return
			}
			resetTimer(idle, server.config.upstreamIdle)

			events, isFinal, err := translateFrame(frame, translator, responses)
			if err != nil {
				streamError(err.Error())
				return
			}
			isComplete = isFinal
			if input.Stream {
				if err := writeEvents(writer, events); err != nil {
					return
				}
			}
		}
	}

	var events []messageEvent
	var err error
	if responses != nil {
		events, err = responses.finish()
	} else {
		events, err = translator.finish()
	}
	if err != nil {
		streamError(err.Error())
		return
	}

	if !input.Stream {
		output, err := translateResponsesMessage(*responses.final, input, messageID)
		if err != nil {
			streamError(err.Error())
			return
		}
		if responses.final.Usage == nil {
			writer.Header().Set("X-Ant2oai-Usage", "unavailable")
		}
		writeJSON(writer, http.StatusOK, output)
		return
	}

	// A failed write means that the client left, so no work remains.
	writeEvents(writer, events)
}

// translateFrame converts one upstream SSE frame to downstream events. It reports
// true when the frame ends the stream. Exactly one protocol is active: responses
// is nil on the Chat Completions route. Errors contain client-safe text.
func translateFrame(
	frame sseFrame, translator *streamTranslator, responses *responsesStream,
) ([]messageEvent, bool, error) {
	if frame.name == "error" && responses == nil {
		return nil, false, errors.New("Upstream sent a streaming error.")
	}

	data := bytes.TrimSpace(frame.data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil, true, nil
	}
	if len(data) == 0 || frame.name == "ping" {
		return nil, false, nil
	}

	if responses != nil {
		events, err := responses.consume(data, frame.name)
		return events, responses.final != nil, err
	}

	var chunk completionResponse
	if checkJSONDepth(data) != nil || json.Unmarshal(data, &chunk) != nil {
		return nil, false, errors.New("Upstream sent an invalid JSON stream event.")
	}
	events, err := translator.consume(chunk)
	return events, false, err
}

// resetTimer restarts a timer that can already have expired. Before Go 1.23 the
// channel must be drained first, or the old expiry would fire immediately.
func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

func writeEvents(writer http.ResponseWriter, events []messageEvent) error {
	for _, event := range events {
		if err := writeEvent(writer, event); err != nil {
			return err
		}
	}
	return nil
}

func writeEvent(writer http.ResponseWriter, event messageEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}

	controller := http.NewResponseController(writer)
	err = controller.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event.Type, data); err != nil {
		return err
	}
	return controller.Flush()
}

func writeAPIError(writer http.ResponseWriter, status int, kind, message, requestID string) {
	// Callers supply proxy-controlled messages. Raw decoder and provider errors
	// must stay out of responses and the completion log.
	failure := apiError{Type: kind, Message: message}
	if recorded, ok := writer.(*statusWriter); ok {
		recorded.failure = &failure
	}
	writeJSON(writer, status, errorResponse{
		Type: "error", Error: failure, RequestID: requestID,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		slog.Error("could not encode JSON response")
		status = http.StatusInternalServerError
		data = []byte(`{"type":"error","error":` +
			`{"type":"api_error","message":"Could not encode response."}}`)
	}

	controller := http.NewResponseController(writer)
	err = controller.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if _, err := writer.Write(append(data, '\n')); err != nil {
		slog.Debug("response write failed")
	}
}

type statusWriter struct {
	http.ResponseWriter
	status  int
	failure *apiError
}

func (writer *statusWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func (writer *statusWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *statusWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(data)
}
