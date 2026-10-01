# 0006 — Two provider adapters: Anthropic Messages and stateless OpenAI Responses

- **Status:** Accepted
- **Date:** 2026-09-30

## Context

The agents must run against a local model (Ollama, for development, evals
and CI replay) and hosted APIs (Anthropic, OpenAI, xAI), behind one neutral
interface (`agent.Model`). Each wire format is an adapter to write, test and
keep current, so the fewer the better. Tickets are customer data, so no
provider should keep conversation state for us.

## Decision

Two adapters, selected by the registry (`internal/providers.New`):

1. **`internal/providers/anthropic`**: Messages API (`POST /v1/messages`,
   `anthropic-version` pinned in `APIVersion`). No `temperature`, no
   `tool_choice` (default auto). Tool results go first in a user turn;
   non-object arguments are echoed as `{}`. Usage maps 1:1 (input is
   already uncached; cache read/creation map to the cache fields).
   `thinking` blocks are ignored: thinking is never enabled.
2. **`internal/providers/openairesp`**: Responses API (`POST /responses`),
   one adapter for OpenAI, xAI and Ollama (different base URL and key).
   - **Stateless:** every call sends the whole conversation with
     `store:false` and never a `previous_response_id`; function calls are
     echoed by `call_id`, without item ids.
   - **Reasoning items are dropped** from the output and never replayed:
     with `store:false` and no encrypted content they could not be.
   - Usage: `input = input_tokens - cached_tokens`, `cache_read =
     cached_tokens`, `reasoning = output_tokens_details.reasoning_tokens`.
   - No `Authorization` header when the key is empty (Ollama); `strict:false`
     on tools, since the loop validates arguments itself.
   - Silent-truncation guard: with `ContextWindow > 0`, input ≥ 90% of it
     returns `ErrContextWindow` (Ollama drops the head of an over-long prompt
     instead of failing).
- Both adapters map stop reasons to the neutral errors of ADR-0003 and
  share `httpjson.Post` (bounded retries, error bodies to `HTTPError`).
- **Fixtures.** Request goldens and responses under each package's
  `testdata/` are **hand-authored from the provider docs as of 2026-09-29**
  (they pin our reading of the wire format, not observed behavior). The one
  recorded fixture is `openairesp/testdata/ollama_qwen3_8b.*`, captured from
  a real local Ollama with `-record` and replayed offline.

## Why not Chat Completions

At design time (2026-09-29) the provider documentation indicated that
Responses is the forward path: OpenAI's GPT-6 models require it for tool
calling, xAI marks Chat Completions as legacy, and Ollama serves
`/v1/responses`. Those statements come from the docs, not from anything
this repository can check; the Ollama part is confirmed by the recorded
fixture.

## Consequences

- Positive: four providers from two adapters; no server-side conversation
  state, so a retry never depends on it and no provider stores our tickets
  through us; wire changes show up as golden diffs (`-update`).
- Negative: resending the whole conversation costs input tokens every step
  (bounded by `MAX_TOKENS`). Dropping reasoning items costs quality on
  reasoning models across tool turns (local models run with reasoning
  `none`). Paid-provider adapters are verified by hand-authored fixtures
  only: no paid key was used, so real-API drift is caught only when someone
  runs one.

## Alternatives considered

- **Chat Completions for OpenAI-compatible providers:** see above.
- **Stateful Responses (`store:true`, `previous_response_id`):** fewer
  tokens, but the provider retains customer text and retries depend on its
  state.
- **Provider SDKs:** three dependencies with their own retry and state
  behavior, for the few fields we use.
- **A single gateway/abstraction library:** hides exactly the stop-reason
  and usage details that ADR-0003 and ADR-0007 depend on.
