# Changelog

## Unreleased

Initial implementation. This release introduces the configuration keys and HTTP
contract described in `README.md`; there is no earlier release to migrate from.

- Translate Anthropic Messages requests to OpenAI Chat Completions.
- Translate normal and streaming responses, with sequential Anthropic content
  blocks for parallel OpenAI function calls.
- Support client-side tool results, images, and exact model-ID mappings.
- Provide local authentication, timeouts, resource limits, and graceful shutdown.
- Use the Go standard library with zero third-party dependencies.
