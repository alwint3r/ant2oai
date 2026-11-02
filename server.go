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

const downstreamWriteTimeout = 30 * time.Second

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
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = cfg.upstreamIdle
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConns = cfg.maxConcurrent
	transport.MaxIdleConnsPerHost = cfg.maxConcurrent
	transport.MaxResponseHeaderBytes = 32 << 10

	return &http.Client{
		Transport: transport,
		// Refuse every redirect so API credentials stay at the configured URL.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
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

func (server *proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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
		writeAPIError(writer, http.StatusInternalServerError, "api_error", "Could not allocate a request ID.", "")
		return
	}
	idSuffix := hex.EncodeToString(randomID[:])
	requestID := "req_" + idSuffix
	writer.Header().Set("request-id", requestID)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	recorded := &statusWriter{ResponseWriter: writer}
	started := time.Now()
	defer func() {
		slog.Info("request completed",
			"request_id", requestID, "status", recorded.status,
			"duration", time.Since(started).Round(time.Millisecond))
	}()
	writer = recorded

	if !server.isAuthorized(request) {
		writeAPIError(writer, http.StatusUnauthorized, "authentication_error",
			"Invalid proxy credential; set ANTHROPIC_AUTH_TOKEN to the configured proxy key.", requestID)
		return
	}
	if request.URL.Path == "/v1/messages/count_tokens" {
		writeAPIError(writer, http.StatusNotFound, "not_found_error",
			"Exact token counting is unavailable for this upstream. "+
				"Claude Code can use its local estimate.", requestID)
		return
	}
	if request.URL.Path != "/v1/messages" {
		writeAPIError(writer, http.StatusNotFound, "not_found_error",
			"Unknown endpoint. Use POST /v1/messages.", requestID)
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "invalid_request_error",
			"This endpoint requires POST.", requestID)
		return
	}

	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "invalid_request_error",
			"Content-Type must be application/json.", requestID)
		return
	}
	if !server.admit() {
		writer.Header().Set("Retry-After", "1")
		writeAPIError(writer, http.StatusTooManyRequests, "rate_limit_error",
			"Proxy request or concurrency limit reached.", requestID)
		return
	}
	defer func() { <-server.slots }()

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
		return
	}
	if err := checkJSONDepth(raw); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}

	var input messagesRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&input)
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
			"Expected one valid Messages request with supported fields. "+
				"Disable experimental betas and check the compatibility guide in README.md.", requestID)
		return
	}

	translated, err := translateRequest(input, server.config)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
		return
	}
	var upstreamInput any = translated
	isChatGPT := server.config.authMode == "chatgpt"
	if isChatGPT {
		converted, err := translateResponsesRequest(translated)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error(), requestID)
			return
		}
		upstreamInput = converted
	}
	payload, err := json.Marshal(upstreamInput)
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Could not encode the translated request.", requestID)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), server.config.requestTimeout)
	defer cancel()
	upstreamURL := server.config.upstreamBaseURL + "/chat/completions"
	credential := server.config.upstreamAPIKey
	// OAuth may renew and persist credentials before starting inference.
	if isChatGPT {
		if server.oauth == nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "api_error",
				"ChatGPT credentials are unavailable; stop the proxy and run ant2oai login.", requestID)
			return
		}
		credential, err = server.oauth.accessToken(ctx)
		if err != nil {
			writer.Header().Set("X-Should-Retry", "false")
			writeAPIError(writer, http.StatusServiceUnavailable, "api_error", err.Error(), requestID)
			return
		}
		upstreamURL = chatGPTResource + "/responses"
	}

	upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(payload))
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Could not create the upstream request.", requestID)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "application/json")
	if input.Stream || isChatGPT {
		upstream.Header.Set("Accept", "text/event-stream")
	}
	if credential != "" {
		upstream.Header.Set("Authorization", "Bearer "+credential)
	}

	// Inbound credentials, beta headers, cookies, and tracing metadata stay local.
	response, err := server.client.Do(upstream)
	if err != nil {
		if request.Context().Err() != nil {
			return
		}
		var networkError net.Error
		isTimeout := errors.Is(ctx.Err(), context.DeadlineExceeded)
		if errors.As(err, &networkError) && networkError.Timeout() {
			isTimeout = true
		}
		if isTimeout {
			writeAPIError(writer, http.StatusGatewayTimeout, "api_error", "Upstream request timed out.", requestID)
		} else {
			writeAPIError(writer, http.StatusBadGateway, "api_error",
				"Could not connect to the upstream. "+
					"Check the base URL, network, and TLS configuration.", requestID)
		}
		return
	}
	defer response.Body.Close()
	if isChatGPT {
		if upstreamID := response.Header.Get("X-Request-Id"); safeUpstreamRequestID(upstreamID) {
			writer.Header().Set("X-Ant2oai-Upstream-Request-Id", upstreamID)
		}
	}

	if response.StatusCode != http.StatusOK {
		server.writeUpstreamError(writer, response, requestID)
		return
	}
	if input.Stream || isChatGPT {
		reportedContentType := response.Header.Get("Content-Type")
		contentType, _, _ := mime.ParseMediaType(reportedContentType)
		// The direct ChatGPT route can omit Content-Type. Accept its SSE with a
		// missing or text/plain type; the bounded parser still validates completion.
		isEventStream := contentType == "text/event-stream"
		if isChatGPT && (reportedContentType == "" || contentType == "text/plain") {
			isEventStream = true
		}
		if !isEventStream {
			writeAPIError(writer, http.StatusBadGateway, "api_error",
				"Upstream did not return a text/event-stream response.", requestID)
			return
		}
		server.serveStream(writer, ctx, response.Body, input, "msg_"+idSuffix, requestID)
		return
	}

	rawResponse, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
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
	output, err := translateResponse(completion, input, "msg_"+idSuffix)
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
	expected := sha256.Sum256([]byte(server.config.proxyAPIKey))
	providedAPIKey := sha256.Sum256([]byte(request.Header.Get("X-Api-Key")))
	var bearer string
	scheme, value, found := strings.Cut(request.Header.Get("Authorization"), " ")
	if found && strings.EqualFold(scheme, "Bearer") {
		bearer = value
	}
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
	if server.config.authMode == "chatgpt" {
		writeChatGPTUpstreamError(writer, response, requestID)
		return
	}
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

		// Match only a known code. Provider messages can contain credentials or
		// prompt fragments, so they are never copied to responses or logs.
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if err == nil && json.Unmarshal(body, &failure) == nil && failure.Error.Code == "context_length_exceeded" {
			message = "Prompt is too long: upstream context window exceeded. " +
				"Compact the conversation or select a larger-context model."
		}
	case http.StatusUnauthorized, http.StatusForbidden:
		message = "Upstream authentication or permission check failed. Check OPENAI_API_KEY and model access."
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

func copyRetryHeaders(writer http.ResponseWriter, response *http.Response) {
	if response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden {
		if retry := response.Header.Get("X-Should-Retry"); retry == "true" || retry == "false" {
			writer.Header().Set("X-Should-Retry", retry)
		}
	}
	if retry := response.Header.Get("Retry-After"); retry != "" {
		seconds, err := strconv.Atoi(retry)
		if err != nil {
			if retryAt, parseError := http.ParseTime(retry); parseError == nil {
				seconds = int(math.Ceil(time.Until(retryAt).Seconds()))
			} else {
				seconds = -1
			}
		}
		if seconds >= 0 && seconds <= 86400 {
			writer.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}
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
		if err := writeEvent(writer, messageEvent{Type: "message_start", Message: &message}); err != nil {
			return
		}
	}

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

	pings := time.NewTicker(15 * time.Second)
	defer pings.Stop()
	idle := time.NewTimer(server.config.upstreamIdle)
	defer idle.Stop()
	translator := newStreamTranslator(input.Tools)
	var responses *responsesStream
	if server.config.authMode == "chatgpt" {
		responses = newResponsesStream(input.Tools)
	}
	streamError := func(message string) {
		slog.Warn("stream failed", "request_id", requestID, "reason", message)
		if input.Stream {
			writeEvent(writer, messageEvent{Type: "error", Error: &apiError{Type: "api_error", Message: message}})
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

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(server.config.upstreamIdle)
			if frame.name == "error" && responses == nil {
				streamError("Upstream sent a streaming error.")
				return
			}

			data := bytes.TrimSpace(frame.data)
			if bytes.Equal(data, []byte("[DONE]")) {
				isComplete = true
				continue
			}
			if len(data) == 0 || frame.name == "ping" {
				continue
			}

			var events []messageEvent
			var err error
			if responses != nil {
				events, err = responses.consume(data, frame.name)
				isComplete = responses.final != nil
			} else {
				var chunk completionResponse
				if checkJSONDepth(data) != nil || json.Unmarshal(data, &chunk) != nil {
					streamError("Upstream sent an invalid JSON stream event.")
					return
				}
				events, err = translator.consume(chunk)
			}
			if err != nil {
				streamError(err.Error())
				return
			}
			if input.Stream {
				for _, event := range events {
					if err := writeEvent(writer, event); err != nil {
						return
					}
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
		output, err := translateResponse(*responses.final, input, messageID)
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
	for _, event := range events {
		if err := writeEvent(writer, event); err != nil {
			return
		}
	}
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
	writeJSON(writer, status, errorResponse{
		Type: "error", Error: apiError{Type: kind, Message: message}, RequestID: requestID,
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		slog.Error("could not encode JSON response")
		status = http.StatusInternalServerError
		data = []byte(`{"type":"error","error":{"type":"api_error","message":"Could not encode response."}}`)
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
	status int
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
