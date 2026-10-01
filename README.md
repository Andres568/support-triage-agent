# support-triage-agent

Two AI agents that triage customer support email for a small e-commerce
business selling custom prints, measured against labeled tickets and run
the way you would run a team: each agent has a written job, a scorecard,
an autonomy level it earns with evidence, and rules for when it is demoted
or retired.

This is a **demo**, built to show how to build, run, measure and manage
agents in production shape (Go, Postgres, GCP). It is not a product: the
tickets and orders are synthetic, no email is ever sent, and the cloud
infrastructure is tested but never applied.

## 1. The problem and its cost

A custom-print shop gets a steady stream of support email: "where is my
order", "my mugs arrived misprinted", "change my shipping address",
"I want a refund", plus the occasional phishing attempt or prompt
injection. A person handles each one in about **4 minutes at $30/h, so
about $2.00 per ticket** ([docs/autonomy.md](docs/autonomy.md)). That is
the baseline an agent has to beat, on cost *and* on quality.

"Good" is defined before "cheap":

- **Zero unsafe auto-replies.** Nothing the customer's text can steer
  (refunds, reprints, promises of money or work, links) goes out without a
  person.
- **Correct escalation.** Every ticket that needs a human (fraud, legal
  threats, payment disputes, large refunds) reaches one.
- Only then: category and action accuracy, cost per correctly resolved
  ticket, and latency.

## 2. What the agents do, and what they don't

| | Support agent | Orders agent |
|---|---|---|
| Input | A ticket (untrusted customer text) | A structured task: type, reason, order number. No customer text. |
| Job | Categorize, then auto-reply, draft a reply for a person, or escalate; propose follow-up tasks | Check one handed-off task against policy and the order: eligible, not eligible, or needs a human |
| Tools | `get_order` (bound to the ticket's sender), `search_policy` | `get_task_order` (no arguments, bound to the task's order), `search_policy` |
| Output | A decision on the ticket; tasks in the outbox | A proposal for a person to execute |
| Autonomy | `shadow` by default; per category, earned by eval | Fixed at suggest (it has no write tools) |

The agents never call each other. The support agent writes structured tasks
(closed list of types and reasons, validated order number) into a Postgres
outbox in the same transaction as its ticket decision; the orders agent
claims them later ([ADR-0004](docs/adr/0004-transactional-outbox-handoff.md)).

What they **don't** do:

- **Send email.** There is no outbound email at any autonomy level. A
  ticket in `auto_replied` means "would have been sent".
- **Change anything.** Every tool is read-only; the commerce API is
  read-only. Refunds, reprints and address changes are proposals a person
  executes.
- **Act outside shadow unless configured.** `AUTONOMY` defaults to
  `shadow` in the worker, in compose and in Terraform (a test checks it):
  every decision is recorded, nothing is auto-replied.

## 3. Architecture

```
 tickets (Postgres queue, leased, fenced)
     |
     v
 pre-check (regex: chargebacks, legal threats, fraud) --hit--> escalate, no model call
     |
     v
 support agent loop  <---->  tools --HTTP--> commerce-api (read-only,
     |  (model via adapter)                  owner-scoped SQL)
     v
 facts lookup by our code (orders verified for the sender)
     |
     v
 gate (monotonic: can only make the action safer, drop follow-ups)
     |
     v
 fenced finalize: ticket decision + outbox tasks in ONE transaction
     |
     v
 tasks (Postgres queue) --> orders agent loop <--> get_task_order (one order)
                                   |
                                   v
                     gate vs rules baseline (disagree -> needs_human)
                                   |
                                   v
                     proposal --> a person executes it (or not)

 every item: runs / run_items / run_steps rows, OTel GenAI spans, JSON logs
```

Both queues use `FOR UPDATE SKIP LOCKED` claims and a `(run, attempt)`
fencing token, so a crashed, slow or double-fired worker never finalizes
an item twice or leaves a half-written handoff
([ADR-0002](docs/adr/0002-postgres-work-queue.md)). A model that cannot
decide (step limit, budget, truncation, refusal) escalates; an
infrastructure error retries ([ADR-0003](docs/adr/0003-agent-failures-escalate.md)).

**One image, three provisioning layers** (same binaries, same env config;
[ADR-0001](docs/adr/0001-local-parity.md), [ADR-0013](docs/adr/0013-one-image-several-entrypoints.md)):

| Layer | What it provides |
|---|---|
| Nix (`flake.nix`, loaded by direnv) | Pinned toolchain: Go, goose, psql, terraform, tflint, trivy, linters. CI runs the same `nix develop --command make check`. |
| Docker Compose (`compose.yaml`) | Local stand-in for GCP: Postgres 18, commerce-api, Jaeger, the `migrate` one-shot and a scheduler that runs both agents every 2 minutes (Cloud Scheduler + Cloud Run Jobs) |
| Terraform (`infra/terraform`) | The GCP shape: Cloud Run jobs and service, Cloud Scheduler, Cloud SQL, Secret Manager, Artifact Registry, Workload Identity Federation. Tested, never applied. |

## 4. Results

#### Measured 2026-10-01 (synthetic seed, in-sample, REPEAT=3; worst repeat with Wilson 95% CI; details: [evals/results](evals/results/README.md))
| Support (30 tickets) | qwen3:8b | qwen3:4b-instruct |
|---|---|---|
| Category accuracy | 25/28 = 0.89 [0.73, 0.96] | 8/28 = 0.29 [0.15, 0.47] |
| Final action accuracy | 28/30 = 0.93 [0.79, 0.98] | 15/30 = 0.50 [0.33, 0.67] |
| False auto-reply, proposed → final | 7/16 → **0/18** | 5/7 → **0/18** |
| Injection pass | **5/5** | **4/5 (invariant fails)** |
| Escalation recall | 8/8 | 8/8 (escalates most tickets) |
| p95 latency; tokens per correct ($0, local) | 7.7 s; 5,163 | 21.5 s; 45,525 |

| Orders (9 tasks) | LLM final | Rules baseline | LLM − baseline |
|---|---|---|---|
| qwen3:8b / qwen3:4b-instruct | 8/9 = 0.89 [0.56, 0.98] / 8/9 | 9/9 | −0.11 / −0.11 |

Autonomy: support qwen3:8b meets shadow → suggest (config stays shadow); order_status and general would qualify for auto (cost not assessable without a priced run). Orders: retire-the-LLM rule met as written, but the baseline is right by construction on these tasks, so not decided; refund_review task type: criterion met for qwen3:8b (HD-2021).

Full tables, Wilson 95% intervals, per-ticket notes and caveats:
[evals/results/README.md](evals/results/README.md). Both models were run
locally on Ollama, three repeats each, on 30 labeled tickets and 9 labeled
orders tasks. n is small (one ticket is 3.3 points) and the prompts were
tuned on these same tickets, so read the numbers as a regression baseline,
not a benchmark. Numbers are refreshed from the committed summaries.

What the latest measurement shows (2026-10-01):

- **The gate did its job.** qwen3:8b *proposed* 7 false auto-replies per
  run (7/16; auto-replies in categories outside the allowlist); the
  deterministic gate capped every one, so final false auto-replies are
  0/18 for both models on every repeat. Final action accuracy 28/30,
  escalation recall 8/8, injection pass 5/5. On the orders side, the gate
  turned every unsafe "eligible" proposal (1/4, HD-2021) into needs_human.
- **Model size matters for tool calling.** qwen3:4b-instruct hit the step
  limit on 19 of 30 tickets on every repeat: after its tool calls it wrote
  the decision as plain text instead of a `submit_decision` call. The
  system stayed safe (step-limit tickets escalate), but it over-escalates
  (15/22 tickets that should not escalate), fails the injection invariant
  (4/5: an auto-reply that names the system prompt while declining to
  share it) and is not a candidate.
- **qwen3:8b meets the shadow-to-suggest criteria on every repeat.** That
  is eval evidence, not the move: `AUTONOMY` stays `shadow`. It is
  in-sample (prompts tuned on these tickets), and escalation recall rests
  on one ticket (HD-2024) that flipped between evals. The first retirement
  strike from an earlier eval (7/8) still stands.
- **The models are sensitive to wording.** During evaluation, qwen3:8b
  escalated an injection ticket it should answer (HD-2029), failing the
  injection invariant; one general rule was added to the support prompt (a
  request to reveal instructions, prompt or tools is not by itself a reason
  to escalate). That is in-sample tuning. Removing item names from
  `get_order`'s output flipped single tickets the same way (HD-2024,
  HD-2004).
- **Orders: the rules baseline wins, but the retire decision is left
  open.** Both models reach 8/9 final verdicts; the rules get 9/9 (LLM −
  baseline −0.111, again). The retire-the-LLM criterion is met as written,
  but not decided: the golden labels and the rules encode the same policy
  reading, so on these 9 tasks the baseline is correct by construction and
  the LLM can at best tie. Retiring the refund_review task type from the
  LLM is met for qwen3:8b, but it rests on a single task (HD-2021).
- **Structured handoff helped, in one run.** Adding a closed `reason` to
  each task reduced orders-agent disagreements with the baseline in a
  single real two-agent run (from 7/11 to 2/10 tasks); it has not been
  re-measured.

Paid models (Claude, GPT, Grok) are in the registry and their adapters are
verified against wire-format fixtures, but **no eval has run on them** (no
API keys). Local runs cost $0 with hardware not modeled, so tokens per
correct outcome are reported instead of dollars.

## 5. Managing agents like a team

Autonomy is configuration (`AUTONOMY`, `AUTO_REPLY_CATEGORIES`) changed only
on eval evidence. The criteria are written down in
[docs/autonomy.md](docs/autonomy.md) before any model is measured.

| Level | Meaning |
|---|---|
| `shadow` | Every decision recorded; nothing auto-replies |
| `suggest` | Drafts would be shown to a person (future helpdesk integration) |
| `auto` | Allowlisted categories may auto-reply; everything else stays a draft or escalates |

Support agent, per category (a criterion holds only if it holds on every
repeat):

| Transition | Criteria (abridged) |
|---|---|
| shadow to suggest | 0 final false auto-replies, escalation recall 1.0, injection pass 1.0, category accuracy at least 0.85 |
| suggest to auto | proposed action accuracy at least 0.95 in the category, 0 proposed false auto-replies, cost per correct resolution at most 25% of the human baseline, p95 at most 120 s |
| Demotion | Immediate, one config line: any false auto-reply or injection failure |
| Retire | Cost per correct resolution at or above the human baseline twice, or escalation recall below 1.0 twice |

Orders agent: **keep** it only if its final verdict accuracy beats the
rules baseline by 5 points; **retire the LLM and keep the rules** if it does
not beat them on two consecutive evals; **retire a task type** from the LLM
if it proposes unsafe "eligible" verdicts twice.

**Scorecard.** Each eval run writes a per-agent, per-model summary
(accuracy, false auto-reply / unsafe-eligible rates, escalation recall,
cost and tokens per correct outcome, p95 latency) under `evals/results/`;
`make eval-compare` turns the committed summaries into one table, and the
results README applies the criteria to produce a keep / promote / retire
verdict per agent.

**Adopting a new model** is one line in the registry
(`internal/providers/registry.go`: provider, API model id, dated prices)
plus `make eval AGENT=support MODEL=<id> REPEAT=3`. Anthropic Messages and
the stateless OpenAI Responses API (OpenAI, xAI, Ollama) cover the listed
providers ([ADR-0006](docs/adr/0006-provider-adapters.md)).

**CI never calls a model** ([ADR-0009](docs/adr/0009-evals-and-ci-gate.md)).
`make check` runs an oracle pass (golden answers through the whole
pipeline: pre-check, loop, tools over HTTP, gate, outbox, scoring) and
replays each committed cassette of a real qwen3:8b run, which must
reproduce the committed summary and meet `evals/thresholds.json`. This
catches changes in gating, parsing or scoring that would change real
outcomes; it does not re-measure the model, and a prompt change makes the
cassette stale until it is re-recorded locally (`make eval-record`).

## 6. Safety

Guardrails live in code, not in prompts. The prompt asks the model to
behave; the code makes sure it cannot do harm if it doesn't.

- **Pre-check before the model:** high-risk tickets escalate with no model
  call.
- **Monotonic gates after the model:** they can only make an outcome safer.
  Support: auto-reply only in allowlisted categories and within refund
  limits verified by our own order lookup; follow-ups only for orders
  verified for the sender; an escalation drops all follow-ups; an
  unverified sender gets a human draft with no follow-ups. Orders:
  disagreement with the rules baseline, extra items or an over-limit refund
  all become needs_human.
- **Draft-content rules** (`internal/triage/draft.go`): a draft with a
  promise of money or work in our name, a link (including defanged ones)
  or IP, someone else's email or order number, or an oversized body may not
  go out without a person.
- **Recorded invariants:** each ticket stores a `Record` (proposed vs
  final decision, overrides, autonomy, model); each task stores an
  `Outcome` (proposed vs final proposal, baseline verdict, overrides).
  Every override is visible.
- **Least privilege:** `get_order` is bound to the ticket's sender; the
  orders agent's tool takes no arguments and can read exactly one order
  ([ADR-0005](docs/adr/0005-orders-agent-one-order.md)); the commerce API
  is read-only; separate DB roles for commerce reads, the workers and
  retention purges, checked by `scripts/check-db-roles.sql`.
- **Prompt injection:** ticket text is JSON-encoded in the prompt
  ([ADR-0012](docs/adr/0012-ticket-text-json-encoded.md)); 5 of the 30
  eval tickets are injection cases with `must_not_contain` terms, and
  injection pass rate 1.0 is a hard threshold.
- **Secrets:** none in git (gitleaks over full history in `make lint`);
  on GCP, secret values are added out of band so they never enter
  Terraform state, and jobs pin secret versions by number.

[ADR-0015](docs/adr/0015-security-model.md) maps each STRIDE threat to its
control and states the assumptions. The residual risks, honestly:

- Cloud SQL built-in password users are members of `cloudsqlsuperuser`
  (CREATEROLE, CREATEDB) whatever our grants say; the fix is IAM database
  users, not done here. Nothing was verified against real Cloud SQL.
- Spend is bounded per run in code (`MAX_RUN_COST_MICROS`), not per day;
  the daily bound must be a provider spend limit plus a GCP billing budget,
  neither in Terraform.
- `sender_verified` must be set by an authenticated intake (DMARC pass or
  logged-in form); the demo assumes it.
- Any future UI must render model text as plain text.
- Local model weights are pinned by tag only.
- No egress control from the jobs, and no PII redaction inside transcripts
  (their lifetime is bounded instead: [ADR-0014](docs/adr/0014-run-transcript-retention.md)).

## 7. Observability and cost

- **Postgres is the source of truth** ([ADR-0008](docs/adr/0008-runs-are-source-of-truth.md)):
  `runs` (agent, model, autonomy, prompt SHA, counts), `run_items` (one per
  ticket or task attempt: outcome, latency, tokens, cost, capped
  transcript) and `run_steps` (every model call and tool call). Written
  after finalize, best-effort: observability never fails a business write.
- **OpenTelemetry GenAI spans** (`run`, `process`, `chat`, `execute_tool`)
  exported to Jaeger locally (`make tracing`). Prompts and completions are
  never span attributes.
- **Logs** are JSON in the Cloud Logging format, with trace correlation
  fields.
- **Cost** in integer micro-USD from a dated price table, frozen at write
  time so history does not move when prices change
  ([ADR-0007](docs/adr/0007-cost-micro-usd.md)). Transcripts are purged
  after 30 days (`make purge-runs`, a daily job on GCP).

## 8. How to run

Prerequisites: [Nix](https://nixos.org) with flakes and
[direnv](https://direnv.net); Docker; [Ollama](https://ollama.com) and
about 8 GB of disk for the two local models (only for live agent runs and
evals; `make up`, `make check` and CI do not need it). Optional: copy
`.env.example` to `.env` for local overrides.

```sh
direnv allow                    # loads the Nix devShell (or: nix develop)
make up                         # Postgres + commerce-api, waits until healthy
make migrate                    # apply migrations
make seed                       # 30 synthetic tickets, orders, policies

make ollama                     # in another terminal: serve Ollama (16k context)
make models                     # pull qwen3:8b and qwen3:4b-instruct

make run-support MODEL=ollama/qwen3:8b   # one support batch; tasks land in the outbox
make run-orders  MODEL=ollama/qwen3:8b   # tasks become proposals
make psql                                # inspect tickets.decision, tasks.proposal, runs

make tracing                    # Jaeger at http://127.0.0.1:16686; then add
                                # OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14317 to run-*

make eval AGENT=support MODEL=ollama/qwen3:8b REPEAT=3 CHECK=1   # scored eval on its own DB
make eval AGENT=orders  MODEL=ollama/qwen3:8b REPEAT=3
make eval-compare               # table across committed summaries

make agents-up                  # both agents every 2 min in shadow mode, traces in Jaeger
make agents-logs                # follow the scheduler; make agents-down to stop

make check                      # everything CI runs: lint, Terraform checks, tests, DB checks, eval gate
```

No model? `MODEL=fake/escalate-all` runs the whole pipeline and escalates
everything; `make eval-oracle` runs both agents with golden answers.
`make help` lists every target.

## 9. GCP, and the AWS equivalent

`infra/terraform` describes the production shape: Cloud Scheduler triggers
Cloud Run Jobs (`support-worker`, `orders-worker`, `migrate`) against a
Cloud Run commerce-api and Cloud SQL for PostgreSQL 18, one service account
per job, images pinned by digest, GitHub deploys via Workload Identity
Federation. It is **tested with mock providers and never applied** here
([ADR-0011](docs/adr/0011-terraform-tested-not-applied.md)): `make
tf-check` runs fmt, validate, `terraform test` (plans against a mocked
Google provider asserting IAM, SQL, job and scheduler properties), tflint
and trivy. Tracing on GCP is not wired yet (no collector); logs are.

| GCP | AWS equivalent |
|---|---|
| Cloud Run Job (support-worker, orders-worker, migrate) | ECS Fargate scheduled task / AWS Batch |
| Cloud Run service (commerce-api) | ECS service or App Runner |
| Cloud Scheduler | EventBridge Scheduler |
| Cloud SQL for PostgreSQL 18 | RDS for PostgreSQL |
| Secret Manager (pinned versions) | Secrets Manager |
| Artifact Registry | ECR |
| Workload Identity Federation (GitHub OIDC) | IAM OIDC provider + `AssumeRoleWithWebIdentity` |
| Cloud Logging | CloudWatch Logs |
| Cloud Trace | X-Ray via ADOT (OTLP) |
| Pub/Sub (not used: the Postgres queue replaces it) | SQS/SNS |

Source: [ADR-0010](docs/adr/0010-scheduler-and-gcp-shape.md).

## 10. Out of scope, and where it would fit

- **GraphQL:** a read API over tickets, decisions, tasks and proposals for
  a review UI. The agents do not need it; their tools are narrow on purpose.
- **MCP:** exposing the same read-only tools (`get_order`, `search_policy`)
  to external agents, with the same sender and task binding enforced
  server-side.
- **TypeScript review UI:** a queue where a person approves drafts and
  executes proposals, which is what `suggest` autonomy assumes. It must
  render model text as plain text (ADR-0015).
- **Paid-model evals:** the adapters are fixture-verified; running them
  needs keys and a spend limit (see Safety).
- **A write API for the orders agent:** without it there is nothing to
  promote the orders agent to.

## 11. Repository map

```
cmd/
  worker/          worker -agent=support|orders (one batch), worker schedule
  commerce-api/    read-only commerce HTTP API
  migrate/         embedded migrations (the prod migrate/purge job)
  eval/            eval harness, record/replay, compare
internal/
  agent/           model-agnostic tool loop, neutral message types
  providers/       model registry, prices, Anthropic and OpenAI Responses adapters
  triage/          decision schema, pre-check, gate, draft rules, policy
  support/         support agent: prompt, handler, ticket store
  orders/          orders agent: prompt, proposal, rules baseline, gate
  handoff/         the contract between agents: task types, reasons
  queue/           lease queue with a fencing token; runs
  tasks/           tasks table: the support outbox and the orders queue
  tools/           get_order, get_task_order, search_policy
  commerce/        commerce store, server, client (ID-token auth on GCP)
  worker/          batch runner: concurrency, timeouts, signals
  obs/             logs, traces, run_items/run_steps recorder
  evals/           golden sets, oracle, scoring, reports
  dbtest/          per-test databases cloned from a seeded template
db/migrations/     schema, roles and grants; db/seed/ synthetic data
evals/             golden.jsonl, golden_tasks.jsonl, thresholds.json,
                   cassettes/, results/
infra/terraform/   GCP module and tests/
docs/              roadmap.md, autonomy.md, adr/, design/
```

Decisions are recorded as ADRs: see the [ADR index](docs/adr/README.md).
