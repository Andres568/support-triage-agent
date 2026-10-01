# Roadmap

A demo of **autonomous AI agents for production** in an e-commerce support
setting: an online store that prints custom products (mugs, T-shirts, posters,
tote bags). Two agents run on a schedule: a support agent reads tickets,
classifies them, looks up orders through tools, and drafts a reply or
escalates to a human; an orders agent re-verifies the follow-up work the
support agent proposes (reprints, address changes, refunds). Every run is
recorded with cost, latency and outcome.

Architecture, trade-offs and results live in the [README](../README.md) and
the [ADRs](adr/README.md); the promotion and retirement criteria are in
[autonomy.md](autonomy.md); eval numbers are in
[evals/results/README.md](../evals/results/README.md).

---

## Principles

1. **Hand-written loop, no agent framework.** What frameworks abstract is
   visible in the code, and nothing is hidden behind one.
2. **Workflow outside, agent inside.** A deterministic state machine owns the
   ticket lifecycle; the LLM tool loop is used only where judgment is needed.
3. **Guardrails live in code, not in the prompt.** The prompt *asks* the model
   to escalate; the runtime *enforces* it.
4. **Structured final decision** (category, action, draft reply, confidence,
   follow-ups). No structure, no evals, no guardrails.
5. **Every agent justifies itself** with numbers a support lead cares about:
   cost per correctly resolved ticket, wrong-escalation rate, time to first
   response.
6. **High-parity local environment.** Everything runs locally; the cloud
   infrastructure is described in Terraform and tested, but not applied.
7. **Nothing installed globally.** Toolchain pinned by a Nix flake; runtime by
   Docker Compose.
8. **Secrets only through environment variables.** Never in the repo.

---

## Phases

| # | Phase | Status |
|---|-------|--------|
| 1 | Foundation: flake, direnv, Compose, Makefile, goose migrations, seed (30 tickets incl. prompt injection), CI through Nix | Done |
| 2 | Workflow, agent loop, tools, guardrails (max steps, timeout, token budget, pre-check and post-loop gate) | Done |
| 2b | Orders agent and handoff: `tasks` queue, `follow_ups` in the decision, outbox insert in the finalize transaction, independent re-verification | Done |
| 3 | Providers: neutral interface, Anthropic and OpenAI-compatible adapters (OpenAI, xAI, Ollama), record/replay cassettes | Done |
| 4 | Observability: runs and steps in Postgres, Cloud-Logging-compatible JSON logs, OpenTelemetry GenAI spans to Jaeger | Done |
| 5 | Evals: golden sets for both agents, Wilson confidence intervals, cost per correct outcome, replay regression gate in CI | Done |
| 6 | Autonomy and production shape: idempotent batches, shadow mode, local scheduler, Terraform with `terraform test` | Done |
| 7 | Review and security hardening: least-privilege DB roles and service accounts, draft-content gate, size and cost limits, supply-chain checks | Done |
| 8 | README and ADRs: business case, architecture, trade-offs, eval table, agent scorecard | Done |
| — | Out of scope: GraphQL commerce API, TypeScript review UI, MCP server, Temporal, pgvector experiment | — |

All phases are re-measured on the final code; see
[evals/results/README.md](../evals/results/README.md).

---

## Key decisions

| Decision | Choice | Why | Alternative |
|----------|--------|-----|-------------|
| Toolchain | Nix flake + direnv | Reproducible, pinned, nothing global | Homebrew, devcontainers |
| IaC | Terraform | Widely known; tested with mocks, never applied | OpenTofu (drop-in) |
| Internal API for tools | Minimal read-only REST `commerce-api` | Tools never touch the database; least privilege | GraphQL, MCP server |
| Local LLM runtime | Ollama, native | GPU access on macOS, OpenAI-compatible API | Docker Model Runner, llama.cpp, LM Studio |
| DB access | pgx + hand-written SQL | Critical queries (`FOR UPDATE SKIP LOCKED`) stay explicit | sqlc |
| Migrations | goose, SQL files, embedded | Fits a separate `migrate` job | golang-migrate |
| Retrieval | `search_policy` over Postgres full-text search | A dozen policies; tool-based retrieval is enough | pgvector hybrid search |
| Loop shape | Deterministic state machine + inner tool loop | Predictable, auditable, retryable | Graph frameworks (LangGraph, ADK, Eino) |
| Multi-agent | Two independent agents joined by a Postgres task queue | Each agent stays testable, measurable and retirable alone | Agents calling agents as tools; A2A |
| Providers | Anthropic + OpenAI-compatible | Two adapters cover every model family used | One adapter per provider |
| Container image | Multi-stage Dockerfile → `distroless/static` | Same image in Compose and Cloud Run | Nix-built image |

---

## Deferred

- Cloud SQL IAM database authentication (replaces role passwords and the
  `cloudsqlsuperuser` residual risk; needs the Go connector).
- OpenTelemetry Collector on GCP for Cloud Trace (logs are wired, traces are
  not).
- A `deploy.yml` workflow using Workload Identity Federation, plus a `prod`
  GitHub environment.
- A GCP billing budget (the daily spend bound; the per-run bound is in code).
- Orders tasks labeled independently of `orders.Baseline`, needed for a real
  retire-the-LLM decision.
- A held-out ticket set (every eval so far is in-sample).
- Paid-model evals (adapters are fixture-verified only; needs keys and a spend
  limit).
