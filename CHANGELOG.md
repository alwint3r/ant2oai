# Changelog

## Unreleased

Initial implementation. This release introduces the configuration keys and HTTP
contract described in `README.md`; there is no earlier release to migrate from.

- Translate Anthropic Messages requests to OpenAI Chat Completions.
- Accept several comma-separated addresses in `listen` (`-listen`,
  `ANT2OAI_LISTEN`, or the JSON key). One HTTP server handles all addresses, so
  they share the proxy key and the concurrency and rate limits. A non-loopback
  address requires a proxy key, which then applies on every address. Startup
  fails if any address cannot be opened. Existing single-address values keep
  working. A listener failure now shuts the proxy down gracefully, exits with
  status 1, and logs the reason.
- Support inline base64 PDF documents in user messages and tool results,
  including existing Claude Code conversation history. Forward the full PDF as
  a Chat Completions `file` or Responses `input_file` part. Tool-result PDFs
  follow all pending tool replies with their tool-call associations preserved.
  Validate base64 and report nested content indices with safe block-type errors.
  PDF title/context metadata, citation options, and other source types remain
  unsupported. Existing text and image requests keep their translations.
- Accept empty terminal Responses `output` arrays when every streamed item has
  a validated `response.output_item.done` snapshot. Preserve response-ID, status,
  content, tool-argument, refusal, and usage validation. Add specific safe errors
  for other completion mismatches. Completion logs now record streaming failure
  reasons even when the HTTP 200 headers have already been sent.
- Support `stop_sequences` in non-streaming ChatGPT Messages requests, including
  Claude Code's client-side auto-mode classifier calls. Match the earliest
  marker after full response validation and return the matched stop sequence.
  Upstream generation and token usage still cover the full reply. Refusals stay
  intact. Streaming ChatGPT stop-sequence requests continue to receive HTTP 400.
- Add safe HTTP failure reasons and proxy/upstream error sources to completion
  logs. Return `X-Ant2oai-Upstream-Status` for API-key upstream HTTP errors too.
  Raw request data and provider errors remain excluded. Give `safeguards`
  rejections an explicit server-side auto-mode compatibility hint, and document
  client-side classifier routing and safety limits.
- Document Claude Code traffic outside `ANTHROPIC_BASE_URL`, telemetry and
  feedback controls, automatic-update effects, the WebFetch safety-check
  exception, and process-level outbound network restrictions.
- Forward Anthropic `output_config.effort` unchanged as the upstream reasoning
  effort: `reasoning_effort` in Chat Completions and `reasoning.effort` in
  ChatGPT mode. Levels are `low`, `medium`, `high`, `xhigh`, and `max`; other
  values receive HTTP 400. Requests without effort are unchanged. Set
  `omit_reasoning_effort` (`-omit-reasoning-effort`) for models without
  reasoning support, which reject the field.
- Translate normal and streaming responses, with sequential Anthropic content
  blocks for parallel OpenAI function calls.
- Support client-side tool results, images, and exact model-ID mappings.
- Provide local authentication, timeouts, resource limits, and graceful shutdown.
- Use the Go standard library with zero third-party dependencies.
- Load optional JSON configuration with defaults → file → environment → flags
  precedence. Existing environment-only and flag-only setups continue to work.
- Add ChatGPT OAuth login, saved account labels, status, model listing, token
  refresh, and logout with best-effort remote revocation. OAuth credentials are
  stored separately from configuration with owner-only permissions on Unix.
- Translate ChatGPT plan-backed inference through the public Responses API,
  including streamed text, tools, and buffered non-streaming Messages replies.
  API-key mode continues to use Chat Completions. ChatGPT mode requires OpenAI
  eligibility and granted plan permission; it never falls back to API-key billing.
- Fix model listing for ChatGPT catalogs larger than 256 KiB. Catalogs have a
  separate 8 MiB limit and specific read, size, and JSON errors. OAuth responses
  retain their 256 KiB limit.
- Allow the local proxy key in a JSON `proxy_api_key` field, with
  `ANT2OAI_API_KEY` taking precedence. Files containing a non-empty key require
  owner-only permissions on Unix. Existing environment-based keys keep working;
  upstream API keys remain environment-only.
- Accept ChatGPT SSE responses with a missing `Content-Type` or a `text/plain`
  header, using the existing bounded stream parser and completion validation.
  Chat Completions streaming continues to require `text/event-stream`.
- Fetch signing keys before refreshing ChatGPT tokens, so temporary JWKS failures
  leave the current grant untouched. ID-token validation uses that key snapshot.
- Use the current authorization request's scopes when a login token response
  omits `scope`. Explicit scope responses remain authoritative.
- Attempt remote logout revocation for access-token-only accounts, with the
  matching token-type hint.
- Accept RSA signing keys whose optional JWK `alg` is absent. JWT headers and
  signature verification remain restricted to RS256.
- Sync the parent credential directory after Unix file replacement and propagate
  sync errors. Windows retains its existing standard-library rename behavior.
