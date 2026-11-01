# ant2oai

A small Go proxy for using **Claude Code with an OpenAI-compatible Chat
Completions server**. It uses the Go standard library and has zero third-party
Go dependencies.

```text
Claude Code  →  POST /v1/messages  →  ant2oai  →  POST /chat/completions
                  Anthropic format                OpenAI format
```

The proxy handles text, streaming, client-side tools, parallel tool calls, and
images. Your upstream model must support the features you use. Coding-agent
quality depends on that model's ability to follow instructions and call tools.

## Build and start

Requires Go 1.22 or later. From this directory:

```sh
make build

# Use your server's API base URL, including its API prefix.
export OPENAI_BASE_URL='http://127.0.0.1:8000/v1'
export OPENAI_MODEL='your-tool-capable-model'

# Omit this variable for a local upstream that needs no authentication.
export OPENAI_API_KEY='your-upstream-api-key'

# Choose a random local token and use the same value in Claude Code.
# For example, generate one with: openssl rand -hex 32
export ANT2OAI_API_KEY='your-random-local-proxy-token'

./bin/ant2oai
```

This starts the proxy at `http://127.0.0.1:8080`. Keep that terminal open.
`OPENAI_BASE_URL` defaults to `https://api.openai.com/v1`. The proxy appends
`/chat/completions` to the configured base URL exactly once. A server mounted at
`/api/v1` therefore needs a base URL ending in `/api/v1`.

Secrets are read from the environment. The proxy does not load `.env` files.

## Connect Claude Code

In a second terminal, set:

```sh
unset ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN
unset CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
unset CLAUDE_CODE_USE_MANTLE CLAUDE_CODE_USE_ANTHROPIC_AWS

export ANTHROPIC_BASE_URL='http://127.0.0.1:8080'
export ANTHROPIC_AUTH_TOKEN='your-random-local-proxy-token'
export ENABLE_TOOL_SEARCH=false
export CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1

claude
```

Use the same local token in both terminals. `ANTHROPIC_BASE_URL` must point to
the proxy root; Claude Code adds `/v1/messages` itself. Check `/status` inside
Claude Code to confirm the base URL and credential source. Settings files can
override your shell environment, so check them if `/status` shows another URL.

`OPENAI_MODEL` routes **all incoming model IDs**, including Claude Code's
background requests, to your chosen upstream model. Claude Code continues to
use its requested model ID in the response. This preserves its client-side
model selection. The actual model is determined by the mapping below.

### Verify the HTTP connection

With the Claude Code variables set, run:

```sh
curl -fsS "$ANTHROPIC_BASE_URL/healthz"

curl -sS -N "$ANTHROPIC_BASE_URL/v1/messages?beta=true" \
  -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"proxy-check","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"Say hello briefly."}]}'
```

The health response is `{"status":"ok"}`. It checks the proxy process only.
A successful streaming inference ends with `message_delta` and `message_stop`.
An `error` event means the completion failed after streaming started.

## Model mapping

Model selection follows this order:

1. An exact incoming-ID match in `ANT2OAI_MODEL_MAP`.
2. The fixed model in `OPENAI_MODEL`.
3. The incoming model ID unchanged.

For separate main and background models, choose explicit local aliases:

```sh
# In the proxy terminal, before starting ant2oai:
export ANT2OAI_MODEL_MAP='{"claude-main":"your-coding-model","claude-small":"your-small-model"}'

# In the Claude Code terminal:
export ANTHROPIC_MODEL='claude-main'
export ANTHROPIC_DEFAULT_OPUS_MODEL='claude-main'
export ANTHROPIC_DEFAULT_SONNET_MODEL='claude-main'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='claude-small'
```

Mappings are exact and case-sensitive. There are no wildcard or substring
matches. If `OPENAI_MODEL` is also set, it remains the fallback for unmapped IDs.
Restart the proxy after changing its environment.

## Configuration reference

Command-line flags override environment values. Durations use Go syntax such
as `30s`, `2m`, or `10m`. Run `./bin/ant2oai -help` for built-in help.

| Flag | Environment variable | Default |
| --- | --- | --- |
| `-listen` | `ANT2OAI_LISTEN` | `127.0.0.1:8080` |
| `-upstream-base-url` | `OPENAI_BASE_URL` | `https://api.openai.com/v1` |
| `-model` | `OPENAI_MODEL` | Pass through incoming model ID |
| `-model-map` | `ANT2OAI_MODEL_MAP` | `{}` |
| `-token-limit-field` | `ANT2OAI_TOKEN_LIMIT_FIELD` | `max_tokens` |
| `-stream-usage` | `ANT2OAI_STREAM_USAGE` | `true` |
| `-omit-sampling` | `ANT2OAI_OMIT_SAMPLING` | `false` |
| `-timeout` | `ANT2OAI_TIMEOUT` | `10m` |
| `-upstream-idle-timeout` | `ANT2OAI_UPSTREAM_IDLE_TIMEOUT` | `2m` |
| `-max-body-bytes` | `ANT2OAI_MAX_BODY_BYTES` | `33554432` (32 MiB) |
| `-max-concurrent` | `ANT2OAI_MAX_CONCURRENT` | `64` |
| `-requests-per-minute` | `ANT2OAI_REQUESTS_PER_MINUTE` | `600` |
| Environment only | `OPENAI_API_KEY` | Empty; no upstream Authorization header |
| Environment only | `ANT2OAI_API_KEY` | Empty; local authentication disabled |

Boolean environment values accept `true` and `false`. Set a false boolean flag
with `-stream-usage=false` or `-omit-sampling=false`.

### Provider differences

For OpenAI models that require `max_completion_tokens` and reject sampling
parameters:

```sh
./bin/ant2oai -token-limit-field=max_completion_tokens -omit-sampling
```

For servers that reject `stream_options`:

```sh
./bin/ant2oai -stream-usage=false
```

The proxy makes one upstream request per incoming request. It performs no
automatic retries or parameter fallback. Claude Code controls retries.

## Compatibility

| Capability | Behavior |
| --- | --- |
| Messages API | `POST /v1/messages`, including query strings such as `?beta=true` |
| Non-streaming | One OpenAI completion choice translated to an Anthropic message |
| Streaming | Incremental SSE with sequential content blocks and a `ping` every 15 seconds |
| System prompt | Text string or text-block array; text blocks joined with blank lines |
| Conversation | User and assistant text, plus mid-conversation system text |
| Tool definitions | Custom Anthropic tools translated to OpenAI function tools; `strict` forwarded when supplied |
| Tool choice | `auto`, `none`, `any` → `required`, and a named tool; parallel-use setting translated |
| Tool history | Assistant `tool_use` and user `tool_result` translated to assistant/tool messages |
| Parallel tools | Interleaved arguments tracked by upstream index; downstream content blocks stay sequential for Anthropic SDK compatibility |
| Tool errors | `is_error` represented by a `Tool error: ` text prefix |
| Images | Base64 JPEG/PNG/GIF/WebP or HTTP(S) URLs translated to `image_url` parts |
| Tool-result images | Tool text retains a marker; images follow all tool replies in a user message with the tool-call ID |
| Stops | `stop` → `end_turn`, `length` → `max_tokens`, tool calls → `tool_use`, content filters → `refusal` |
| Usage | Upstream prompt/completion counts translated; cached prompt tokens separated into `cache_read_input_tokens` |
| Startup probe | `HEAD /api/hello` returns 204 |
| Health | `GET /healthz` and `HEAD /healthz`, without authentication |

### Limits and unsupported features

- The upstream must implement **Chat Completions**. Responses API-only servers,
  Bedrock signing, Vertex authentication, and Azure-specific `api-key`
  authentication are outside this implementation.
- Set `ENABLE_TOOL_SEARCH=false`. Anthropic server-managed tools, deferred
  tools, and `tool_reference` blocks receive HTTP 400. Claude Code's own local
  tools, including MCP tools loaded upfront, use the normal function-call path.
- Tool names must contain 1–64 ASCII letters, digits, underscores, or dashes.
  Rename longer MCP tools at their source or use shorter server/tool names.
- Anthropic `thinking` settings and historical thinking blocks are omitted.
  The upstream uses its own reasoning defaults. Provider-specific reasoning
  deltas are ignored. The proxy does not create signed thinking blocks.
- Prompt-cache markers, metadata, and `output_config.effort` are omitted.
  Upstream automatic prompt caching can still operate. `service_tier: auto`
  is accepted and omitted.
- Structured output formats, task budgets, context-management requests,
  `top_k`, assistant prefill, documents/PDFs, audio, and unsupported content
  block types receive HTTP 400. Unknown top-level fields are rejected.
  `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` removes several incompatible
  Claude Code request fields.
- `POST /v1/messages/count_tokens` returns an Anthropic-shaped HTTP 404.
  Claude Code can use its character-based estimate. Chat Completions has no
  portable exact token-counting endpoint.
- Token usage is zero when the upstream omits it. A non-streaming response
  also carries `X-Ant2oai-Usage: unavailable` in this case. Streaming starts
  with zero usage and reports available upstream counts in the final
  `message_delta`. These values are never presented as tokenizer estimates.
- OpenAI does not identify which stop string matched. The proxy returns
  `stop_sequence: null` and maps a normal `stop` finish to `end_turn`.
- Model discovery (`/v1/models`) and Anthropic account/billing endpoints are
  unavailable. Use explicit model aliases if needed.
- Every tool's arguments must finish as a JSON object. Invalid or interrupted
  tool arguments produce an error. Streaming failures never emit a successful
  `message_stop`. A clean EOF after an upstream finish reason is accepted;
  EOF before that finish reason is an error.

## Security and resource limits

The proxy binds to loopback by default. Local authentication is optional on
loopback; enabling it is recommended on shared machines. Other listen
addresses require `ANT2OAI_API_KEY`. Both `Authorization: Bearer ...` and
`x-api-key: ...` are accepted for local authentication.

The upstream credential comes exclusively from `OPENAI_API_KEY`. Inbound
credentials, cookies, beta headers, and tracing headers are never forwarded.
Upstream redirects are refused. Logs include generated request IDs, status,
duration, and safe stream errors. Request bodies, response bodies, and API keys
are excluded. Upstream error bodies are replaced by safe messages because
providers may include credentials or prompt fragments in them.

Incoming HTTP has no TLS. Use a trusted TLS reverse proxy or an SSH tunnel
for connections across machines. Keep the upstream on HTTPS or a trusted local
network. Standard Go `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` behavior applies
to outgoing requests. A configured network proxy is part of your trust boundary.
The process has no persistence, analytics, provider SDK, or background model calls.

Inference admission is process-wide: a token bucket refills at the configured
requests per minute, with a burst equal to `max-concurrent`. Active requests
also cannot exceed that concurrency limit. Excess requests receive HTTP 429
without entering a queue. Health and startup probes bypass these limits.

Additional bounds:

- Incoming headers: 32 KiB limit, 5-second read-header timeout.
- Incoming body: 30-second read timeout and configurable size limit.
- JSON nesting: 128 levels.
- Upstream connection and TLS handshake: 10 seconds each.
- Upstream response headers and streaming idle: configurable, 2 minutes by default.
- Total upstream request: configurable, 10 minutes by default; covers body reads.
- Idle upstream connections: closed after 90 seconds.
- Upstream JSON response or total SSE input: 64 MiB.
- One SSE event: 2 MiB. One tool's arguments: 8 MiB.
- Tool calls per response/message: 128. Downstream content blocks per stream: 1024.
- Downstream writes: 30 seconds per event or JSON response.

Streams use a bounded one-frame channel for backpressure. Text before tool use
and the first tool's arguments are forwarded immediately. Arguments for other
parallel calls and any text after tool use are buffered until the upstream
finishes. This keeps downstream content blocks sequential for Anthropic SDK
callbacks. All tool arguments are retained for validation within the limits
above. A downstream disconnect cancels the upstream request. `SIGINT` and
`SIGTERM` allow active requests 15 seconds to finish, then cancel them.

Pointing Claude Code at this proxy changes its model-request route. Claude Code
can also make other network requests. Set
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` if you want to suppress its optional
traffic; this also disables automatic updates. That variable does not cover
every network feature, including the WebFetch domain safety check.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Local HTTP 401 | Use the proxy token as `ANTHROPIC_AUTH_TOKEN`; check `/status` for settings overrides |
| Upstream authentication failure | Check `OPENAI_API_KEY` and upstream model permissions |
| Upstream endpoint/model missing | Check the `/v1` or other API prefix in `OPENAI_BASE_URL`, then the selected model |
| Upstream rejects the request | Check tool support, model output/context limits, token-limit field, and sampling restrictions |
| Tool search or beta-field HTTP 400 | Set the two compatibility variables in the Claude Code example and start a new session |
| Context window exceeded | Run `/compact`; choose a model with enough context for Claude Code's prompt |
| Output budget rejected | Set Claude Code's `CLAUDE_CODE_MAX_OUTPUT_TOKENS` below the upstream model's output limit |
| Stream stalls or times out | Check reverse-proxy buffering and the upstream idle/total timeouts |
| Local HTTP 429 | Reduce parallel agents or adjust the local admission limits |

The `request-id` response header links a failure to the proxy's completion log.
The proxy deliberately excludes raw provider error messages. Check the upstream
server's own diagnostics when a safe error message needs more context.

## Development

```sh
make fmt    # Format Go source
make build  # Build bin/ant2oai
make lint   # Check formatting and run go vet
make test   # Run go test ./...
go test -race ./...
```

The repository currently has no test files. Follow `AGENTS.md` before adding
tests. `main.go` owns process lifetime, `config.go` loads configuration,
`server.go` handles HTTP and admission, and `protocol.go` defines wire types.
The request, response, and stream translation files contain the protocol logic.
The stream translator can consume parsed chunks independently of HTTP.
