package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// Match documented codes only. Provider messages and detail fields can contain
// secrets or prompt fragments, so they never become downstream diagnostics.
func chatGPTFailureMessage(code string) (string, bool) {
	switch code {
	case "subscription_sharing_user_not_eligible":
		return "ChatGPT plan usage is unavailable for this account or workspace " +
			"(subscription_sharing_user_not_eligible). Check OpenAI eligibility and workspace policy.", true
	case "subscription_sharing_usage_limit_exceeded":
		return "ChatGPT app or plan usage limit reached (subscription_sharing_usage_limit_exceeded). " +
			"Review https://chatgpt.com/settings/usage before sending more requests.", true
	case "subscription_sharing_usage_unavailable", "subscription_sharing_user_unavailable":
		return "ChatGPT usage or account information is temporarily unavailable. " +
			"Credentials were retained; retry later.", true
	case "subscription_sharing_unsupported_capability":
		return "ChatGPT rejected an unsupported capability (subscription_sharing_unsupported_capability). " +
			"Check the selected model, input, and tool schemas.", true
	case "subscription_sharing_route_not_supported":
		return "ChatGPT denied the Responses route (subscription_sharing_route_not_supported). " +
			"Check whether OpenAI has enabled this integration.", true
	case "subscription_sharing_invalid_user":
		return "ChatGPT could not validate the selected account (subscription_sharing_invalid_user). " +
			"Check granted scopes and sign in again if the session was revoked.", true
	case "chatpass_v2_scope_not_authorized", "chatpass_v2_invalid_authorization_context":
		return "ChatGPT plan permission does not authorize this request. " +
			"Check the selected registration and granted scopes.", true
	case "context_length_exceeded":
		return "Prompt is too long: upstream context window exceeded. " +
			"Compact the conversation or select a larger-context model.", true
	default:
		return "ChatGPT request failed. Check account permissions, model access, " +
			"OpenAI availability, and the upstream status and request-ID headers.", false
	}
}

func writeChatGPTUpstreamError(writer http.ResponseWriter, response *http.Response, requestID string) {
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	errorType := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
		errorType = "invalid_request_error"
	case http.StatusUnauthorized:
		errorType = "authentication_error"
	case http.StatusForbidden:
		errorType = "permission_error"
	case http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	}

	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 || checkJSONDepth(raw) != nil ||
		json.Unmarshal(raw, &failure) != nil {
		failure.Error.Code = ""
	}
	message, known := chatGPTFailureMessage(failure.Error.Code)
	writer.Header().Set("X-Ant2oai-Upstream-Status", strconv.Itoa(response.StatusCode))
	if known {
		writer.Header().Set("X-Ant2oai-Upstream-Code", failure.Error.Code)
	}
	copyRetryHeaders(writer, response)
	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusBadRequest || failure.Error.Code == "subscription_sharing_usage_limit_exceeded" {
		writer.Header().Set("X-Should-Retry", "false")
	}
	writeAPIError(writer, status, errorType, message, requestID)
}

func safeUpstreamRequestID(value string) bool {
	if len(value) < 5 || len(value) > 128 || value[:4] != "req_" {
		return false
	}
	for _, character := range value[4:] {
		isLetter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '-' && character != '_' {
			return false
		}
	}
	return true
}
