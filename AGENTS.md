# AGENTS.md

This file guides everyone who changes this repository, including human
contributors and AI agents. Read it before your first change.

A more specific instruction takes priority over this file. Examples are a
nested `AGENTS.md`, a code comment that describes a local constraint, and an
explicit request from the maintainer.

The goals, in priority order:

1. **Readable:** a maintainer can understand the code by following its
   execution and data flow.
2. **Maintainable:** a change in one place has predictable, limited effects.
3. **Efficient on real hardware:** performance is measured on the machines
   that run the code.

Readability comes first because readable code can be optimized safely later.
Code that nobody understands is risky to change, and that includes changes made
for performance.

---

## 1. Project context

- **Purpose:** an HTTP proxy that accepts Anthropic Messages API requests and
  sends them to an OpenAI Chat Completions API upstream or to the public
  Responses API with ChatGPT OAuth. It translates the responses, including
  streaming responses, back to the Anthropic format.
- **Language:** Go. `go.mod` declares `go 1.22`, so the code must compile with
  Go 1.22. A newer local toolchain can accept language features and standard
  library APIs that Go 1.22 lacks, so check the Go version before you use one.
- **Dependencies:** standard library only. Follow section 5 before you add a
  module.
- **Build:** `make build` produces `bin/ant2oai`.
- **Format:** `make fmt` formats the Go source. For focused maintenance, format
  only changed files with `gofmt -w <file>` to avoid unrelated changes.
- **Lint:** `make lint` checks formatting and runs `go vet ./...`.
- **Test:** `make test` runs `go test ./...`. Also run `go test -race ./...`
  when a change touches concurrency. Tests live next to the code in
  `*_test.go` files, and shared fakes live in `helpers_test.go`. Follow section
  8 before you add or change them.
- **Benchmarks:** no benchmark suite exists yet.
- **Naming in Go:** the examples in section 4 use generic spelling. In Go code,
  use Go conventions: `camelCase` for unexported names, `PascalCase` for
  exported names, and initialisms in one case (`upstreamURL`, `requestID`).

Configuration and compatibility are documented in `README.md`. `main.go` owns
process lifetime, `config.go` and `config_file.go` load settings, `server.go`
owns HTTP I/O and admission, and `protocol.go` defines Messages and Chat
Completions wire types. `translate_request.go`, `translate_response.go`, and
`stream.go` contain their protocol translation. `responses.go` adapts the
ChatGPT Responses protocol. Stream translators accept parsed chunks or events
independently of network I/O. `oauth.go` owns login commands, `oauth_oidc.go`
verifies ID tokens, `oauth_session.go` owns token renewal, and `oauth_store.go`
owns protected credential files and process locks. OAuth tokens must only go to
the discovered OpenAI auth endpoints and https://api.openai.com/v1.
JSON configuration may store a local proxy key in `proxy_api_key`. Such files
are local secret stores and require owner-only permissions on Unix. Keep real
keys out of committed examples and configuration.

---

## 2. Writing style

This applies to code comments, documentation, commit messages, and pull
request descriptions.

- Write for a technically competent reader who may not be a native English
  speaker.
- Use clear international English: short sentences, common words, and explicit
  logical connections such as "because", "therefore", and "so".
- Avoid idioms, slang, uncommon metaphors, and unnecessarily sophisticated
  vocabulary.
- Keep necessary technical terms. Explain unfamiliar terms briefly the first
  time they appear.
- Keep the technical content complete. Simplify the language only.
- State the intended meaning directly. Avoid rhetorical contrast patterns such
  as "X is Y, not Z" and "not X, but Y". When a distinction matters, describe
  the concrete role, behavior, or scope of each item. Keep necessary warnings
  and prohibitions.

---

## 3. How to work

1. **Understand before editing.** Read the surrounding code, its callers, and
   its tests. Follow the existing conventions, even when you would have chosen
   differently.
2. **Keep changes small and focused.** Make one logical change per commit. Do
   not reformat, rename, or clean up unrelated code in the same change.
3. **Write concrete code first.** Do not add interfaces, classes,
   configuration options, or architectural layers for hypothetical future
   needs.
4. **Verify before you report.** Build, lint, and run the existing tests before
   you say that a change is complete. If something fails or you skipped a step,
   say so clearly.
5. **Ask when a decision is really ambiguous.** Ask about product behavior,
   public API shape, and any action that is destructive or hard to reverse. For
   other decisions, choose a sensible option and state what you chose.

---

## 4. Readability

Prioritize understanding. Line count, reuse, and compliance with a design
doctrine are secondary.

- **Names describe domain meaning and purpose.** Use `pending_requests` instead
  of `list2`. Include units or lifecycle stages when they matter:
  `idle_timeout_ms`, `raw_body`, `parsed_request`. Write booleans as questions:
  `is_streaming`, `has_tools`. Avoid abbreviations unless they are universal
  (`id`, `url`, `ctx`).
- **Prefer straightforward control flow.** Use explicit intermediate values
  instead of dense expressions, deeply nested conditionals, or clever chaining.
  Extra lines are useful when they expose meaning.
- **Limit nesting.** Use early returns and guard clauses for invalid input and
  error cases. Then the main success path reads from top to bottom.
- **Keep related logic together, in execution order where practical.** See
  "Stages and spacing" below for how to make the stages visible.
- **Make important behavior visible.** A reader must be able to find state
  changes, side effects, error paths, and resource lifetimes easily. Do not
  hide them inside innocent-looking expressions or helpers.
- **Comment on intent, constraints, invariants, and non-obvious reasoning.**
  Do not restate the code in prose. Update comments when the code changes.
- **Name constants and include their units:** `MAX_BODY_BYTES`,
  `CONNECT_TIMEOUT_MS`.
- **Use the project formatter.** Do not format by hand or discuss style in
  review.

### Stages and spacing

A formatter fixes indentation and alignment only. You must show the structure
of a function yourself, with blank lines and short comments.

- **Separate each stage of a function with one blank line.** A stage is a group
  of statements that do one step together, for example "parse flags",
  "validate the upstream URL", or "build the response".
- **Add a one-line comment above a stage when its purpose is not obvious from
  its code.** The comment names the step or states its constraint, for example
  `// Upstream URL: absolute, no credentials, no trailing slash.`
- **Keep the statements inside one stage together.** Do not put a blank line
  after every statement or every `if` block. That adds height and still hides
  the structure.
- **Treat a function of more than about 25 lines with no blank lines as a
  review signal.** It usually contains several stages that the reader cannot
  see. Add spacing and stage comments first. Split the function only if it is
  still hard to follow after that.
- **Keep lines short enough to read without horizontal scrolling,** about 100
  characters. When a line is longer, move long literals such as help text into
  a named constant or a table, or break the line at a logical point.

### Functions and helpers

- A long, coherent function can be clearer than many small functions that
  force the reader to jump between them. Treat function length as a reason to
  review the code. It is never a rule by itself.
- A private helper with one call site is appropriate when it isolates a
  coherent operation and makes the caller meaningfully easier to understand.
  Do not extract helpers only to shorten a function or to wrap obvious code.
- Extract shared code only after at least two real call sites show the same
  meaning. Similar syntax alone is insufficient evidence.
- Do not apply DRY, SOLID, "single responsibility", or function-size rules
  mechanically. Judge each change by one question: does it make the actual
  program easier to understand and modify?

---

## 5. Maintainability

- **Module boundaries.** Each module owns one clear concern and exposes a small
  surface. Keep network, disk, clock, and environment access in clearly marked
  places, so that the core logic can run with supplied inputs.
- **Explicit dependencies.** Pass dependencies and context objects as
  arguments. Do not keep mutable state in hidden globals or singletons.
  Explicit context supports multiple instances and isolated tests.
- **One source of truth.** Do not keep a copy of state that another component
  owns, unless protocol state, caching, or measured performance requires it.
  When you keep a copy, make invalidation explicit.
- **Parse at the boundary.** Validate untrusted input once, convert it into
  well-typed structures, and rely on those types inside the program. Where the
  language allows it, design types so that invalid states cannot be
  constructed.
- **Errors carry context.** Never ignore an error silently. Add context as the
  error propagates, for example `decode tool_call arguments: <cause>`. Make
  partial success explicit. Separate client errors (invalid input) from server
  errors (a bug in this code or an upstream failure), and map each to the
  correct status code or exit code.
- **Delete dead code.** Remove unused functions, flags, and commented-out code.
  Version control keeps the history.
- **Add dependencies carefully.** Each dependency adds maintenance, security,
  and build cost. Add one only when it saves significant work, is actively
  maintained, and has a compatible license. Pin its version. For small tasks,
  use the standard library.
- **Public interfaces are contracts.** A change to an API, wire format, CLI
  flag, or configuration key needs a deliberate decision, a changelog entry,
  and a migration path for existing users.

---

## 6. API design

Apply this section when other code uses your code across a module or package
boundary. It does not require reusable components or extra layers in ordinary
application code.

The goal is that callers can integrate the API into their real program without
workarounds, rewrites, or adopting an unrelated control model.

### Start from real use

- Write the concrete call sites before you design the abstraction.
- Check the proposed API against three call sites: the simplest case, a
  realistic production case, and an awkward edge case.
- Add convenience wrappers last. They must be optional, and callers must be
  able to build them from the same public primitives.

### Let the caller keep control

- Let the caller own the main loop, threads, scheduling, retry policy, and
  cancellation policy.
- Offer bounded operations such as `step`, `process`, or `poll` when they make
  integration and deterministic testing easier.
- Do not make callbacks, background threads, inheritance, or a framework-owned
  event loop the only way to integrate.
- Keep mechanism and policy separate, unless the policy is part of the domain.
- Document whether each operation blocks, how long it can block, and how the
  caller sets a timeout or cancels it.

### Make ownership explicit

- For every context, buffer, configuration, and handle, define its owner, its
  lifetime, how it is allocated, and who may change it.
- Where memory policy matters, prefer caller-owned buffers and explicit
  allocators over hidden allocation.
- Make the required initialization order visible in types, parameters, or
  documentation.
- Make failures and partial results explicit and actionable. Do not rely on
  undocumented sentinel values or side channels.

### Provide useful layers

When the domain needs layers, build them in this order:

1. **Primitives:** small, independent operations with explicit state and
   effects.
2. **Semantic operations:** domain concepts built from primitives.
3. **Convenience helpers:** common compositions of semantic operations.

A caller that outgrows a helper must be able to use the lower layers without
abandoning the API.

**Warning signs:** lists of boolean arguments (use a named options structure),
hidden allocation, callback-only designs, global initialization, undocumented
call order, retained copies of caller state, mandatory background threads, and
a growing set of special-case functions.

**Example for this project:** a stream translator can be a state object that
accepts one upstream chunk and returns zero or more downstream events. The HTTP
layer then owns I/O, timeouts, and cancellation. Tests and benchmarks can feed
chunks directly, without a network.

---

## 7. Performance and hardware efficiency

Treat performance work as measurement: measure, change one thing, then measure
again.

### 7.1 Process

- Write a benchmark or capture a profile before you optimize. Record the
  baseline and the result in the pull request.
- Optimize only the measured hot path, which is the code that runs most often
  or takes most of the run time. In all other code, prefer clarity.
- Profile on hardware similar to production: the same CPU architecture, a
  similar core count, and similar memory. Apple Silicon (arm64) and x86_64
  behave differently, so results from one may not apply to the other.
- Never trade correctness for speed. Every optimization must keep all tests
  passing.

### 7.2 Algorithms and data structures first

- Choose the right algorithmic complexity before micro-optimizing. An O(n)
  algorithm usually outperforms a heavily tuned O(n²) algorithm.
- Choose data structures for the access pattern: contiguous arrays for
  iteration and hash maps for lookup. Avoid linked structures in hot loops.

### 7.3 Memory and CPU caches

- **Allocate less in hot paths.** Reuse buffers, preallocate when the capacity
  is known, and avoid temporary strings and collections for each item.
- **Keep data contiguous and compact.** Sequential memory access uses the CPU
  cache well and lets the hardware prefetcher load data early. When a loop
  reads only a few fields, consider a struct-of-arrays layout.
- **Avoid unnecessary copies.** Pass references, use slices or views, and
  stream data when you do not need the full payload in memory.
- **Avoid false sharing.** False sharing happens when threads write to
  different values on the same CPU cache line, which slows all of them. Pad or
  shard counters that different threads update often.

### 7.4 Concurrency and parallelism

- Size worker pools from the runtime environment: available cores and
  container CPU limits. Never hard-code a thread count.
- Use asynchronous, non-blocking I/O for network-bound work. Use threads or
  processes for CPU-bound work. Never block an async executor with CPU-heavy
  work or synchronous I/O.
- Minimize shared mutable state. Prefer message passing, or per-thread state
  that is merged at the end. Keep lock scopes short, and never hold a lock
  across I/O or an `await`.
- Give every queue and buffer a size limit. When the limit is reached, apply
  backpressure (slow down or reject the producer) so that memory cannot grow
  without limit.

### 7.5 I/O and networking

- Stream large or incremental data, such as SSE or chunked responses, from end
  to end. Forward each chunk when it arrives instead of buffering the full body.
- Reuse connections with HTTP keep-alive and connection pools. Set explicit
  connect, read, and idle timeouts on every outbound call.
- Batch small writes and system calls. Use buffered readers and writers.
- When the downstream client disconnects, cancel the related upstream work.

### 7.6 Build and runtime

- Benchmark and ship optimized release builds. Debug builds do not represent
  production performance.
- Do not use host-specific CPU flags such as `-march=native` for distributed
  binaries unless you also ship a portable build.
- Use SIMD or other hardware-specific code only through well-tested libraries,
  or behind a clear interface with a portable fallback. Test both paths.
- Keep logging in hot paths cheap. Use log levels, skip formatting work when a
  level is disabled, and never log full request or response bodies by default.

---

## 8. Testing

### Rules for AI agents

- Do not add, modify, or generate tests unless the maintainer explicitly asks
  for test changes. This includes test files, fixtures, snapshots, test
  helpers, benchmarks, and test-only configuration.
- You may run existing tests and benchmarks to verify a change.
- If verification needs new test code and nobody asked for tests, write a
  disposable test or harness in an operating-system temporary directory outside
  the repository. Remove it after verification.

### Standards for test code

When someone writes or requests tests, follow these standards:

- A bug fix includes a regression test that fails without the fix.
- Test behavior through public interfaces. Avoid testing private internals.
- Tests are fast, deterministic, and isolated. They use no real network, no
  sleeps, and no dependence on wall-clock time or test order. Supply clocks and
  fake upstream servers as dependencies.
- Tests never touch real credentials or default ports, even when the code under
  test is broken. `TestMain` replaces the home directory. A disposable harness
  or mutation run needs the same protection.
- Cover edge cases: empty input, maximum sizes, malformed input, partial or
  interrupted streams, timeouts, and concurrent access.
- For format translation, prefer table-driven tests or golden files that pair
  each input with its expected output.

---

## 9. Security and robustness

- Treat all external input as untrusted. Validate its size, type, and shape at
  the boundary. Enforce limits on body size, nesting depth, and request rate.
- Never log or return secrets such as API keys, authorization headers, or
  tokens. Redact them in logs and error messages.
- Load secrets from the environment or a secret store. Never put them in
  source code or committed configuration.
- When the program reaches an unexpected state, return an error. Do not guess
  a result.

---

## 10. Commits and pull requests

- Commit subject: imperative mood, at most 72 characters, for example "Add
  streaming tool-call translation". The body explains why the change is needed.
- A pull request describes the problem, the approach, how it was verified, and
  any performance numbers before and after the change.
- Do not commit generated files, build output, local configuration, or
  secrets.

---

## 11. Before finishing

Review the change before you report it as complete:

- [ ] **Trace the main path and one important failure path.** A reader can see
      what happens, which data changes, and where errors go, without jumping
      through unrelated files.
- [ ] Confusing names, dense expressions, deep nesting, and unnecessary
      indirection are simplified. This review did not turn into unrelated
      cleanup.
- [ ] Each function shows its stages with blank lines, and stages with a
      non-obvious purpose have a one-line comment. No long function is a
      single block of statements.
- [ ] Comments and documentation follow the writing style in section 2.
- [ ] The code builds with no new warnings.
- [ ] The formatter, linter, and existing tests pass.
- [ ] Hot-path changes include benchmark numbers from before and after.
- [ ] No debug output, dead code, or TODO without an owner or issue remains.
- [ ] Documentation and this file are updated if behavior or workflow changed.
- [ ] The diff contains only what the change needs.
