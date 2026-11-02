# Changelog

## Unreleased

Initial implementation. This release introduces the configuration keys and HTTP
contract described in `README.md`; there is no earlier release to migrate from.

- Translate Anthropic Messages requests to OpenAI Chat Completions.
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
