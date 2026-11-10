# ant2oai

A small Go proxy for using **Claude Code with an OpenAI-compatible Chat
Completions server or an eligible ChatGPT account**. It uses the Go standard
library and has zero third-party Go dependencies.

```text
Claude Code → POST /v1/messages → ant2oai ┬→ Chat Completions (API key)
                                         └→ Public Responses API (ChatGPT OAuth)
```

The proxy handles text, streaming, client-side tools, parallel tool calls,
images, and inline PDFs. Your upstream model must support the features you use.
Coding-agent quality depends on that model's ability to follow instructions and
call tools.

## Build and start with an API key

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

The upstream API key is read from `OPENAI_API_KEY`. The local proxy key can
come from `ANT2OAI_API_KEY` or a protected [JSON configuration file](#json-configuration).
The proxy does not load `.env` files.

## Sign in with ChatGPT

ChatGPT mode uses OpenAI's documented
[ChatGPT plan usage flow for open-source clients](https://developers.openai.com/siwc/token-sharing-open-source/sign-in).
OpenAI controls account, workspace, region, model, and integration eligibility.
A successful identity sign-in can still lack permission for plan-backed
inference. Review app limits and credit settings at
[ChatGPT Settings → Usage](https://chatgpt.com/settings/usage).

```sh
make build
cp ant2oai.chatgpt.example.json ant2oai.chatgpt.json

# Clear overrides left over from an API-key setup.
unset OPENAI_API_KEY OPENAI_BASE_URL OPENAI_MODEL ANT2OAI_MODEL_MAP ANT2OAI_AUTH_MODE
export ANT2OAI_API_KEY='your-random-local-proxy-token'

./bin/ant2oai login -config ant2oai.chatgpt.json
./bin/ant2oai models -config ant2oai.chatgpt.json
```

Open the printed **Continue with ChatGPT** URL in your browser. Login waits up
to five minutes for an HTTP callback at `127.0.0.1` on a temporary port. The
browser must be able to reach that loopback listener; a remote shell requires
an SSH tunnel for the printed callback port. ant2oai keeps its access and refresh
tokens in the terminal process and its protected credential file. The browser
callback contains an authorization code, which is exchanged using PKCE, a
per-attempt proof key.

Choose a model slug from `models`, replace `your-chatgpt-model-slug` in
`ant2oai.chatgpt.json`, then start:

```sh
./bin/ant2oai -config ant2oai.chatgpt.json
```

Connect Claude Code using the [same local proxy settings below](#connect-claude-code).
Keep `ENABLE_TOOL_SEARCH=false`. The incoming Claude model IDs are routed by
`model` and `model_map` in both authentication modes.

Useful commands, with the command placed before its flags:

```sh
./bin/ant2oai login-status -config ant2oai.chatgpt.json

# Stop the proxy before changing credentials.
./bin/ant2oai login -config ant2oai.chatgpt.json -reconsent
./bin/ant2oai logout -config ant2oai.chatgpt.json

# Keep a separate registration for another account or workspace.
./bin/ant2oai login -config ant2oai.chatgpt.json -account work
./bin/ant2oai -config ant2oai.chatgpt.json -account work
```

`login-status` lists saved labels, emails, granted plan permission, and access
expiry. It reads local state; it does not confirm current OpenAI eligibility.
To make a label the default, set `account` in the JSON file or
`ANT2OAI_ACCOUNT`. Up to 32 account registrations can share one credential
file. Registrations stay separate even when their emails match.

Credentials default to `ant2oai/chatgpt.json` under Go's OS user-config
directory: `~/Library/Application Support` on macOS, `$XDG_CONFIG_HOME` or
`~/.config` on Linux, and `%AppData%` on Windows. Override the full file path
with `auth_file`, `-auth-file`, or `ANT2OAI_AUTH_FILE`. Keep this file outside
your repository. On Unix, directories created by ant2oai use `0700`, and
credential files use `0600`. Existing credential files with wider permissions
are rejected. Windows installations must protect the directory with user-only
access-control permissions.

The proxy refreshes credentials on demand near access-token expiry. It fetches
OpenAI's signing keys before exchanging the refresh token. A temporary key-fetch
failure leaves the existing tokens saved so a later request can retry.

Replacement tokens are saved together. On Unix, the temporary file is synced,
renamed over the credential file, and followed by a parent-directory sync before
success is reported. A directory-sync failure is reported even when the new
file is already visible. Windows syncs the temporary file and uses Go's rename
operation; atomic replacement and directory-entry durability are not guaranteed.

A `<auth-file>.lock` file allows one proxy or credential-writing command at a
time. Stop the proxy before `login`, `logout`, or `models`; `login-status` can
run while it is serving. On macOS, Linux, and the BSDs, the process holds an
`flock(2)` lock on this file, and the kernel releases the lock when the process
exits for any reason. Therefore a `.lock` file left by a killed or crashed
process does not block the next start. On other platforms, including Windows,
only the file's existence locks the credentials. After a crash there, verify the
old process has stopped before removing its stale `.lock` file. Logout attempts to revoke the refresh
token, or the access token when no refresh token is saved. It clears local
tokens and retains the host ID and account/client mapping. If remote revocation
is unconfirmed, disconnect the app in ChatGPT Settings.

### ChatGPT-specific limits

- Inference uses `POST https://api.openai.com/v1/responses`. Custom upstream
  base URLs are rejected in ChatGPT mode so OAuth tokens stay at OpenAI.
- Every upstream inference request uses `store: false` and `stream: true`, with the
  required history sent in `input`. Non-streaming Messages clients receive a
  buffered JSON response after stream validation finishes. The direct route can
  omit `Content-Type`. ant2oai accepts SSE with a missing or `text/plain` header
  and applies the same stream limits and Responses completion checks. If the
  terminal event contains an empty `output` array, the proxy uses the validated
  `response.output_item.done` snapshots. Every streamed item must have a complete
  snapshot. Response-ID, status, content, tool-argument, refusal, and usage checks
  remain in place.
- System text becomes developer messages. Functions are supplied through
  developer `additional_tools` input items. Text, images, inline PDFs, tool
  choices, parallel calls, and tool-result history use the Responses equivalents.
  PDFs use `input_file` with inline data; the proxy does not use the Files
  upload API.
- OpenAI's direct route rejects output-token caps and sampling parameters.
  Anthropic `max_tokens` remains required and validated. Its limit is
  **not enforced in ChatGPT mode**. `temperature` and `top_p` are omitted.
  `token_limit_field`, `stream_usage`, and `omit_sampling` affect API-key mode.
- Non-streaming Messages requests support `stop_sequences` through local
  matching after the full upstream stream passes validation. The proxy returns
  text before the earliest match, `stop_reason: "stop_sequence"`, and the
  matched `stop_sequence`. Upstream generation still finishes in full, and
  usage includes that full reply. Refusals retain their full text and stop
  reason. Streaming Messages requests with `stop_sequences` receive HTTP 400.
- `output_config.effort` becomes `reasoning.effort` with the same level.
  `omit_reasoning_effort` removes it in this mode too.
- OpenAI reasoning state is omitted when translating back to Messages, so it
  cannot be replayed on the next turn. The full supported text and tool
  history is sent each time. This can affect reasoning continuity and usage.
- A grant must include `chatgpt.tokens.use.direct` and `resource.invoke`
  before inference. Use `login -reconsent` to request plan permission again.
  If the token response omits `scope`, login uses the current authorization
  request's scopes, as OAuth defines for an unchanged grant. An explicit scope
  response replaces those scopes. Earlier authorizations supply no fallback.
  ChatGPT mode never switches to API-key billing after a failure.
- Confirmed terminal refresh errors clear unusable tokens while retaining the
  registration. Network and temporary service failures preserve credentials.
  Stop the proxy and sign in again when the session is expired or revoked.

See OpenAI's [preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)
for current route constraints. Live sign-in and inference require your account
and remain subject to OpenAI's service policy.

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
export CLAUDE_CODE_AUTO_MODE_SERVER=0

claude
```

Set `ANTHROPIC_AUTH_TOKEN` to the proxy key configured through `ANT2OAI_API_KEY`
or JSON `proxy_api_key`. `ANTHROPIC_BASE_URL` must point to
the proxy root; Claude Code adds `/v1/messages` itself. Check `/status` inside
Claude Code to confirm the base URL and credential source. Settings files can
override your shell environment, so check them if `/status` shows another URL.

`OPENAI_MODEL` routes **all incoming model IDs**, including Claude Code's
background requests, to your chosen upstream model. Claude Code continues to
use its requested model ID in the response. This preserves its client-side
model selection. The actual model is determined by the mapping below.

For privacy controls and connections that bypass ant2oai, see
[Claude Code traffic outside the proxy](#claude-code-traffic-outside-the-proxy).

### Claude Code auto mode

[Auto mode](https://code.claude.com/docs/en/permission-modes#eliminate-prompts-with-auto-mode)
uses a separate classifier to review tool actions. Recent Claude Code releases
can request server-side review through a `safeguards` request field.
Chat Completions and ChatGPT Responses do not supply Anthropic safety verdicts.
The proxy rejects `safeguards` with HTTP 400 and a configuration hint. Keep
`CLAUDE_CODE_AUTO_MODE_SERVER=0` and
`CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` in the Claude Code terminal, then
start a new session. See Anthropic's
[classifier compatibility guidance](https://code.claude.com/docs/en/auto-mode-classifier-billing).

In an isolated local check, Claude Code 2.1.316 sent non-streaming classifier
requests to `/v1/messages?beta=true`, requesting `claude-sonnet-5`, a 64-token
budget, and `stop_sequences: ["</severity>"]`. Setting
`CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` still permitted those classifier
calls. ChatGPT mode handles their stop markers locally in the buffered reply. This avoids the earlier HTTP 400 rejection for
all stop-sequence requests. Matching also works when a marker spans upstream
stream events. Streaming stop-sequence requests remain unsupported.

Classifier requests use the same `model_map` and fallback `model` as other
requests. A fixed `model` also routes these checks to your OpenAI model. With
only `model_map` configured, include the classifier's requested ID or set a
fallback `model` so it reaches a model your upstream serves. Classifier IDs can
change with Claude Code releases.

**Safety limit:** classifier decisions come from the selected upstream model.
Their safety quality has not been evaluated against Anthropic's classifier.
The proxy does not generate approval verdicts or implement Anthropic's
server-side safety checks. In ChatGPT mode, the classifier's `max_tokens` budget
is also unenforced, so its full reply can increase latency and usage. For
sensitive work, or a Claude Code release that requires server-side review,
use manual approval with `claude --permission-mode default`.

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

1. An exact incoming-ID match in `model_map` (`-model-map` or `ANT2OAI_MODEL_MAP`).
2. The fixed `model` (`-model` or `OPENAI_MODEL`).
3. The incoming model ID unchanged.

For separate main and background models, choose explicit local aliases:

```sh
# In the proxy terminal, before starting ant2oai:
export ANT2OAI_MODEL_MAP='{"claude-main":"your-coding-model","claude-small":"your-small-model"}'

# In the Claude Code terminal:
export ANTHROPIC_MODEL='claude-main'
export ANTHROPIC_DEFAULT_OPUS_MODEL='claude-main'
export ANTHROPIC_DEFAULT_SONNET_MODEL='claude-small'
export ANTHROPIC_DEFAULT_HAIKU_MODEL='claude-tiny'
```

Mappings are exact and case-sensitive. There are no wildcard or substring
matches. If `model` is also set, it remains the fallback for unmapped IDs.
The configuration precedence below applies to both settings. Restart the
proxy after changing its configuration.

## JSON configuration

Configuration precedence, from lowest to highest:

1. Built-in defaults.
2. A JSON file selected by `-config` or `ANT2OAI_CONFIG`.
3. Environment variables.
4. Command-line flags.

No configuration file is loaded automatically. `-config` overrides
`ANT2OAI_CONFIG`; an explicit `-config=` disables file loading. Restart the
proxy after changing settings.

```sh
cp ant2oai.example.json ant2oai.json
# Edit upstream_base_url and model for your provider.
export ANT2OAI_CONFIG="$PWD/ant2oai.json"
export OPENAI_API_KEY='your-upstream-api-key'
export ANT2OAI_API_KEY='your-random-local-proxy-token'
./bin/ant2oai

# A flag can override one file setting.
./bin/ant2oai -config ant2oai.json -listen 127.0.0.1:8081
```

JSON keys match the flags below with underscores replacing dashes.
`proxy_api_key` is an additional JSON-only string. All keys are optional.
`model_map` is an object, booleans are JSON booleans, limits are
integers, and durations are strings such as `"30s"` or `"10m"`. Unknown keys,
`null`, invalid value types, files larger than 1 MiB, and excessive nesting
are rejected. Relative file paths use the process working directory. JSON
paths do not expand `~` or environment variables.

You can include model mappings and the local proxy key in the same file:

```json
{
  "model": "your-default-model",
  "model_map": {
    "claude-main": "your-coding-model",
    "claude-small": "your-small-model"
  },
  "proxy_api_key": "your-random-local-proxy-token"
}
```

Add these fields to your local configuration. Select real upstream model IDs
or ChatGPT model slugs, and generate a random proxy key. Use the same key as
Claude Code's `ANTHROPIC_AUTH_TOKEN`. The mapping keys must exactly match the
model IDs or aliases Claude Code sends.

A file containing a non-empty `proxy_api_key` is a local secret store. On Unix,
it must be a regular file with no group or other access. Before adding a key,
protect the file with `chmod 600 ant2oai.json` (use your actual config path).
On Windows, configure user-only file and directory access-control permissions.
Keep real keys out of source control and shared directories. The local files
`ant2oai.json`, `ant2oai.chatgpt.json`, and `examples/config.json` are ignored
by Git.

`ANT2OAI_API_KEY` overrides `proxy_api_key` whenever the variable is set,
including an empty value. Unset it to use the file key. `ANT2OAI_MODEL_MAP`
similarly replaces the entire file mapping when set. Files containing a
non-empty key still require protected permissions when an environment value
overrides that key.

The upstream API key remains environment-only through `OPENAI_API_KEY`.
OAuth tokens use the separate protected credential file. `config` and the
login-only `reconsent` flag are unavailable as JSON fields. The repository includes
[`ant2oai.example.json`](ant2oai.example.json) and
[`ant2oai.chatgpt.example.json`](ant2oai.chatgpt.example.json).

## Configuration reference

Durations use Go syntax such as `30s`, `2m`, or `10m`.
Run `./bin/ant2oai -help` for built-in help. Plain `ant2oai` and
`ant2oai serve` both start the proxy.

| Flag or JSON-only key | Environment variable | Default |
| --- | --- | --- |
| `-config` | `ANT2OAI_CONFIG` | No configuration file |
| `-auth-mode` | `ANT2OAI_AUTH_MODE` | `api-key`; `chatgpt` selects OAuth Responses |
| `-auth-file` | `ANT2OAI_AUTH_FILE` | OS config directory + `ant2oai/chatgpt.json` |
| `-account` | `ANT2OAI_ACCOUNT` | `default` |
| `-listen` | `ANT2OAI_LISTEN` | `127.0.0.1:8080` |
| `-upstream-base-url` | `OPENAI_BASE_URL` | `https://api.openai.com/v1` |
| `-model` | `OPENAI_MODEL` | Pass through incoming model ID |
| `-model-map` | `ANT2OAI_MODEL_MAP` | `{}` |
| `-token-limit-field` | `ANT2OAI_TOKEN_LIMIT_FIELD` | `max_tokens` |
| `-stream-usage` | `ANT2OAI_STREAM_USAGE` | `true` |
| `-omit-sampling` | `ANT2OAI_OMIT_SAMPLING` | `false` |
| `-omit-reasoning-effort` | `ANT2OAI_OMIT_REASONING_EFFORT` | `false` |
| `-timeout` | `ANT2OAI_TIMEOUT` | `10m` |
| `-upstream-idle-timeout` | `ANT2OAI_UPSTREAM_IDLE_TIMEOUT` | `2m` |
| `-max-body-bytes` | `ANT2OAI_MAX_BODY_BYTES` | `33554432` (32 MiB) |
| `-max-concurrent` | `ANT2OAI_MAX_CONCURRENT` | `64` |
| `-requests-per-minute` | `ANT2OAI_REQUESTS_PER_MINUTE` | `600` |
| `-reconsent` (login only) | — | `false` |
| Environment only | `OPENAI_API_KEY` | Empty; API-key mode sends no Authorization header |
| `proxy_api_key` (JSON only) | `ANT2OAI_API_KEY` | Empty; local authentication disabled |

Boolean environment values accept `true` and `false`. Set a false boolean flag
with `-stream-usage=false` or `-omit-sampling=false`.

`listen` accepts several addresses in one comma-separated string. This works
the same way in the flag, the environment variable, and the JSON key:

```sh
./bin/ant2oai -listen '127.0.0.1:8080,[::1]:8080,100.64.0.1:8080'
```

All addresses use the same settings, and they share the proxy key and the
`max-concurrent` and `requests-per-minute` limits. The proxy does not start if
any address cannot be opened. If one listener fails later, the proxy stops
accepting requests on all addresses, lets active requests finish, and exits
with status 1.

### Provider differences

These flags apply to API-key Chat Completions mode. For OpenAI models that
require `max_completion_tokens` and reject sampling parameters:

```sh
./bin/ant2oai -token-limit-field=max_completion_tokens -omit-sampling
```

For servers that reject `stream_options`:

```sh
./bin/ant2oai -stream-usage=false
```

For models without reasoning support, or upstreams that reject
`reasoning_effort`:

```sh
./bin/ant2oai -omit-reasoning-effort
```

`omit-reasoning-effort` applies to both API-key and ChatGPT mode.

The proxy makes one upstream inference request per incoming Messages request.
Inference has no automatic retries or parameter fallback. Claude Code controls
retries. OAuth renewal and logout can make additional authentication requests.

## Compatibility

The table describes API-key Chat Completions mode. ChatGPT mode uses the
Responses equivalents and [additional limits](#chatgpt-specific-limits).

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
| Effort | `output_config.effort` (`low`, `medium`, `high`, `xhigh`, `max`) forwarded unchanged as `reasoning_effort`; other values receive HTTP 400 |
| Images | Base64 JPEG/PNG/GIF/WebP or HTTP(S) URLs translated to `image_url` parts |
| PDFs | Base64 `application/pdf` document blocks translated to `file` parts; upstream and model must support PDF inputs |
| Tool-result attachments | Tool text retains markers; images and PDFs follow all tool replies in a user message with the tool-call ID |
| Stops | `stop` → `end_turn`, `length` → `max_tokens`, tool calls → `tool_use`, content filters → `refusal` |
| Usage | Upstream prompt/completion counts translated; cached prompt tokens separated into `cache_read_input_tokens` |
| Startup probe | `HEAD /api/hello` returns 204 |
| Health | `GET /healthz` and `HEAD /healthz`, without authentication |

### Limits and unsupported features

- API-key upstreams must implement **Chat Completions**. ChatGPT OAuth uses
  the public Responses API with the restrictions above. Generic Responses-only
  servers, Bedrock signing, Vertex authentication, and Azure-specific `api-key`
  authentication are outside this implementation.
- Set `ENABLE_TOOL_SEARCH=false`. Anthropic server-managed tools, deferred
  tools, and `tool_reference` blocks receive HTTP 400. Claude Code's own local
  tools, including MCP tools loaded upfront, use the normal function-call path.
- Tool names must contain 1–64 ASCII letters, digits, underscores, or dashes.
  Rename longer MCP tools at their source or use shorter server/tool names.
- Anthropic `thinking` settings and historical thinking blocks are omitted.
  Only `output_config.effort` controls upstream reasoning. Without it, the
  upstream uses its own reasoning defaults. The proxy forwards effort levels
  without checking model support, so a model that lacks a level, such as
  `max`, rejects the request. Provider-specific reasoning deltas are ignored.
  The proxy does not create signed thinking blocks.
- Prompt-cache markers, metadata, and per-message `output_config.effort` are
  omitted.
  Upstream automatic prompt caching can still operate. `service_tier: auto`
  is accepted and omitted.
- PDF documents require `source.type: "base64"` and
  `source.media_type: "application/pdf"`. Base64 is validated and the full PDF
  data is forwarded. PDF title/context metadata and citation options receive
  HTTP 400. URL, file-ID, text, and other document sources remain unsupported.
  The incoming body limit includes the encoded PDF and conversation history;
  the upstream can impose additional file-size, page-count, and context limits.
- Structured output formats, task budgets, context-management requests,
  `top_k`, assistant prefill, audio, and unsupported content block types receive
  HTTP 400. Unknown top-level fields are rejected.
  `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` removes several incompatible
  Claude Code request fields.
- `POST /v1/messages/count_tokens` returns an Anthropic-shaped HTTP 404.
  Claude Code can use its character-based estimate. Chat Completions has no
  portable exact token-counting endpoint.
- Token usage is zero when the upstream omits it. A non-streaming response
  also carries `X-Ant2oai-Usage: unavailable` in this case. Streaming starts
  with zero usage and reports available upstream counts in the final
  `message_delta`. These values are never presented as tokenizer estimates.
- In API-key Chat Completions mode, the upstream does not identify which stop
  string matched. The proxy returns `stop_sequence: null` and maps a normal
  `stop` finish to `end_turn`. ChatGPT mode reports locally matched sequences
  for non-streaming Messages replies as described above.
- The proxy's HTTP API omits model discovery (`/v1/models`) and Anthropic
  account/billing endpoints. The `models` CLI command lists account-specific
  ChatGPT models. Use explicit model aliases in Claude Code if needed.
- Every tool's arguments must finish as a JSON object. Invalid or interrupted
  tool arguments produce an error. Streaming failures never emit a successful
  `message_stop`. Chat Completions accepts clean EOF only after an upstream
  finish reason. ChatGPT mode requires `response.completed` or a supported
  `response.incomplete` event.

## Security and resource limits

The proxy binds to loopback by default. Local authentication is optional on
loopback; enabling it is recommended on shared machines. Other listen
addresses require a proxy key from `ANT2OAI_API_KEY` or JSON `proxy_api_key`.
When `listen` has several addresses, one non-loopback address is enough to
require the key, and clients on every address must then send it.
Both `Authorization: Bearer ...` and
`x-api-key: ...` are accepted for local authentication.

API-key mode reads its upstream credential from `OPENAI_API_KEY`. ChatGPT
mode reads its selected saved OAuth token set. Inbound credentials, cookies,
beta headers, and tracing headers are never forwarded.
Upstream redirects are refused. Logs include generated request IDs, status,
duration, safe HTTP failure reasons, and safe stream errors. Request bodies,
response bodies, API keys, and OAuth tokens are excluded. Upstream error bodies
are replaced by safe messages because providers may include credentials or
prompt fragments in them.

Incoming HTTP has no TLS. Use a trusted TLS reverse proxy or an SSH tunnel
for connections across machines. Keep the upstream on HTTPS or a trusted local
network. Standard Go `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` behavior applies
to outgoing requests. A configured network proxy is part of your trust boundary.
The proxy has no analytics, provider SDK, or background model calls. ChatGPT
mode persists OAuth registrations and tokens. Conversation bodies are held
only for request translation and validation; ant2oai does not save them.

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
- Upstream inference JSON response or total SSE input: 64 MiB.
- OAuth metadata, signing keys, and token JSON responses: 256 KiB.
- ChatGPT model-catalog JSON: 8 MiB, including instruction and capability metadata.
- One SSE event: 2 MiB. One tool's arguments: 8 MiB.
- Tool calls per response/message: 128. Downstream content blocks per stream: 1024.
- Downstream writes: 30 seconds per event or JSON response.

Streams use a bounded one-frame channel for backpressure. Text before tool use
and the first tool's arguments are forwarded immediately. Arguments for other
parallel calls and any text after tool use are buffered until the upstream
finishes. This keeps downstream content blocks sequential for Anthropic SDK
callbacks. All tool arguments are retained for validation within the limits
above. A downstream disconnect cancels the upstream request. `SIGINT`,
`SIGTERM`, and `SIGHUP` allow active requests 15 seconds to finish, then cancel
them. A terminal sends `SIGHUP` when it closes. A process started with `SIGHUP`
ignored, for example under `nohup`, keeps ignoring it. OAuth renewal gets up to
five extra seconds to finish credential writes. If it cannot finish, the
process keeps its credential lock until it exits.

### Claude Code traffic outside the proxy

`ANTHROPIC_BASE_URL` routes Claude Code's model API requests, including the
client-side auto-mode classifier calls observed in our local check. Claude Code
also has separate network paths that can contact Anthropic and other services.
The auto-mode and experimental-beta variables in the connection example control
request compatibility. Telemetry has its own controls.

Depending on the Claude Code version, authentication, settings, and enabled
features, these separate paths can include:

| Traffic | Separate destinations or behavior |
| --- | --- |
| Usage metrics | Anthropic and Datadog endpoints |
| Feature-flag evaluation | `api.anthropic.com` |
| WebFetch domain safety checks | Requested hostnames sent to `api.anthropic.com` |
| Authentication and account operations | `claude.ai`, `platform.claude.com`, and related sign-in hosts |
| Version checks and updates | Distribution endpoints such as `downloads.claude.ai` |
| Feedback submissions | Reports to Anthropic can include conversation history and code |
| Plugins, MCP servers, and tools | Their configured services and download hosts |

These requests bypass ant2oai's model-request route and may still use a
configured HTTP proxy. ant2oai's logs cover requests that reach ant2oai. Use
system-level network monitoring to inspect Claude Code's other connections.

Anthropic describes routine metrics as latency, reliability, and usage data,
with code, prompts, and file paths excluded. Feedback submissions have their
own content-sharing behavior. The destinations and controls here come from
Anthropic's published documentation; this project has not performed a full
live-session traffic audit. Review the upstream guidance after Claude Code
updates:

- [Traffic outside the gateway path](https://code.claude.com/docs/en/llm-gateway-connect#turn-off-traffic-outside-the-gateway-path)
- [Telemetry and data usage](https://code.claude.com/docs/en/data-usage#telemetry-services)
- [Network access requirements](https://code.claude.com/docs/en/network-config#network-access-requirements)

#### Disable telemetry and feedback while keeping automatic updates

In the Claude Code terminal, before starting a new session:

```sh
unset CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
export DISABLE_TELEMETRY=1
export DISABLE_ERROR_REPORTING=1
export DISABLE_FEEDBACK_COMMAND=1
```

If `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` is also set in a Claude Code
settings file, remove that entry to retain automatic updates.
`DISABLE_TELEMETRY` also disables feature-flag fetching and session quality
surveys. Features that depend on feature flags can become unavailable.

#### Disable nonessential background traffic

For the broader opt-out, set this before starting a new Claude Code session:

```sh
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
```

This disables telemetry, error reporting, feedback, feature-flag fetching, and
several background checks. **It also disables automatic updates.** Arrange
regular manual updates when using it. WebFetch safety checks, official plugin
marketplace auto-install, and tool or MCP network access have separate controls.

#### WebFetch domain safety checks

Before fetching a URL, WebFetch can send its hostname to `api.anthropic.com`
for a domain-blocklist check. Anthropic documents the payload as the hostname
alone, with the full URL, path, and page contents excluded. This check runs
across model providers and remains active when nonessential traffic is disabled.

To prevent this connection, disable WebFetch or merge the following root-level
field into Claude Code's `~/.claude/settings.json`:

```json
{
  "skipWebFetchPreflight": true
}
```

This removes the domain-blocklist check. Combine it with
[WebFetch domain permission rules](https://code.claude.com/docs/en/permissions#webfetch)
when you need to restrict which hosts the tool can reach.

#### Enforce outbound network access

For enforced destination restrictions, use process-level firewall rules or
container network policies. Allow Claude Code to reach the local proxy and
explicitly approved tool destinations. Give the separate ant2oai process access
to its configured upstream and, in ChatGPT mode, the required OpenAI
authentication endpoints. Verify the policy with network monitoring after
configuration changes and Claude Code upgrades.

## Troubleshooting

### Repeated tool-result errors after reading a PDF

Claude Code can return a whole PDF as a `document` block inside a tool result.
Earlier ant2oai builds rejected that block. The result stays in conversation
history, so subsequent prompts and retries can produce the same HTTP 400.

Update and restart ant2oai with the same configuration, then retry in the
existing Claude Code session. The proxy now translates inline base64 PDFs,
including those already stored in tool-result history. Clearing the conversation
is unnecessary for this compatibility error. The selected upstream and model
must accept PDF inputs.

When the upstream lacks PDF support, read explicit page ranges with Claude
Code's `Read` tool to obtain images, or extract text with `pdftotext` through
Bash. The image route requires an image-capable model. These alternatives affect
new tool results; existing PDF blocks still require a PDF-capable upstream or
conversation history that excludes those blocks.

### Other errors

| Symptom | Check |
| --- | --- |
| Local HTTP 401 | Use the proxy token as `ANTHROPIC_AUTH_TOKEN`; check `/status` for settings overrides |
| Upstream authentication failure | In API-key mode, check `OPENAI_API_KEY`; in ChatGPT mode, check `login-status`, plan scopes, and workspace policy |
| ChatGPT sign-in succeeds but plan usage is disabled | Stop the proxy and run `login -reconsent`; check OpenAI eligibility and ChatGPT app permissions |
| Credential file is locked | Stop the process using it. On platforms without `flock(2)`, remove a stale `.lock` only after verifying that process has stopped |
| ChatGPT HTTP 403 or 429 | Check the safe upstream error code and ChatGPT Settings → Usage; an app-specific restriction or limit can apply |
| Upstream endpoint/model missing | Check the `/v1` or other API prefix in `OPENAI_BASE_URL`, then the selected model |
| Upstream rejects the request | Check tool support, model output/context limits, token-limit field, and sampling restrictions |
| Tool search or beta-field HTTP 400 | Set the compatibility variables in the Claude Code example and start a new session |
| Auto mode classifier HTTP 400 | Check the logged `reason`; use `CLAUDE_CODE_AUTO_MODE_SERVER=0` and the beta setting above; rebuild and restart for non-streaming ChatGPT stop-marker support |
| Context window exceeded | Run `/compact`; choose a model with enough context for Claude Code's prompt |
| Output budget rejected | Set Claude Code's `CLAUDE_CODE_MAX_OUTPUT_TOKENS` below the upstream model's output limit |
| Stream stalls or times out | Check reverse-proxy buffering and the upstream idle/total timeouts |
| Local HTTP 429 | Reduce parallel agents or adjust the local admission limits |

The `request-id` response header links a failure to the proxy's completion log.
HTTP failures include a proxy-controlled `reason` and `error_type` in that log.
`error_source=proxy` means the proxy generated the error, including validation
and translation failures. `error_source=upstream` means the upstream returned an
HTTP error; `upstream_status` records its original status. The proxy deliberately
excludes raw decoder and provider error messages.

Streaming sends HTTP 200 before the final upstream result is known. A later
failure is delivered as an SSE `error` event and recorded in the completion
log's `reason`, even when its HTTP `status` is 200. Failed streams do not emit a
successful `message_stop`. Buffered requests can still return HTTP 502 when
completion validation fails. An empty terminal Responses `output` array is
accepted only after all streamed items have validated item-done snapshots;
other output-count and response-ID mismatches have specific safe diagnostics.

`X-Ant2oai-Upstream-Status` is returned for upstream HTTP errors in both modes.
In ChatGPT mode, HTTP error statuses are preserved, and an allowlisted
`X-Ant2oai-Upstream-Code` and a validated `X-Ant2oai-Upstream-Request-Id`
provide additional safe diagnostics when available. Bodies shaped as `detail`
receive a generic message. Check the upstream server's diagnostics or provide
the safe request ID to OpenAI when more context is needed. Temporary failures do not
trigger a billing-mode fallback.

## Development

```sh
make fmt    # Format Go source
make build  # Build bin/ant2oai
make lint   # Check formatting and run go vet
make test   # Run go test ./...
go test -race ./...
```

Tests run without external network access. They use fake upstreams, fixed dates,
and loopback sockets only for the login callback and listener checks. Follow
`AGENTS.md` when you add or change tests.

`main.go` owns process lifetime, `config.go` and `config_file.go` load
configuration, and `server.go` handles HTTP and admission. The request,
response, and stream translation files contain the Chat Completions protocol
logic; `responses.go` adapts the ChatGPT Responses protocol. The `oauth*.go`
files handle login, ID-token verification, protected storage, and renewal.
Stream translation runs independently of HTTP.
