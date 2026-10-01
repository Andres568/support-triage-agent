# Run inside the Nix devShell (direnv loads it on cd, or `nix develop -c make <target>`).

# .env (via direnv) normally provides this; the default keeps `make` usable without it.
DATABASE_URL ?= postgres://triage:triage@127.0.0.1:15432/triage?sslmode=disable
# Throwaway databases for `make check`, so checks never touch your dev data.
# Unique per invocation (creation time + random), so two runs cannot clobber
# each other and later runs can drop a crashed run's leftovers by age.
CHECK_RUN_ID       := $(shell date +%s)_$(shell od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
CHECK_DB           := triage_check_$(CHECK_RUN_ID)
CHECK_DATABASE_URL :=postgres://triage:triage@127.0.0.1:15432/$(CHECK_DB)?sslmode=disable
# Migrated + seeded copy of the check DB; internal/dbtest clones it per test.
TEST_TEMPLATE_DB   := triage_tmpl_$(CHECK_RUN_ID)

MIGRATIONS_DIR := db/migrations
SEED_FILE      := db/seed/seed.sql
# Model for run-support and run-orders: a registry id (internal/providers), e.g. ollama/qwen3:8b or fake/escalate-all.
MODEL ?= ollama/qwen3:8b
# Days of run_items (transcripts: customer text and order details) that purge-runs keeps.
RETENTION_DAYS ?= 30

.DEFAULT_GOAL := help

.PHONY: help up tracing down reset ollama models agents-up agents-down agents-logs psql logs migrate migrate-down migrate-status seed purge-runs check tf-check lint test db-check run-support run-orders eval-db eval eval-record eval-replay eval-oracle eval-compare

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-16s %s\n", $$1, $$2}'

up: ## Start local services (rebuilding changed images) and wait until healthy
	docker compose up -d --build --wait

tracing: ## Start Jaeger for OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14317 (UI http://127.0.0.1:16686)
	docker compose --profile tracing up -d --wait jaeger

down: ## Stop local services (keeps data)
	docker compose --profile tracing --profile agents down

reset: ## Stop local services and delete their data
	docker compose --profile tracing --profile agents down -v

# --- Agents on a schedule (docs/adr/0010) --------------------------------------
# The worker-scheduler container runs both agents every SCHEDULE_EVERY in shadow
# mode against Ollama on the host. `make up` and CI never need Ollama.
SCHEDULE_EVERY ?= 2m

ollama: ## Serve Ollama on the host with the context the agents need (foreground)
	OLLAMA_CONTEXT_LENGTH=16384 ollama serve

models: ## Pull the local models the agents and evals use
	ollama pull qwen3:8b
	ollama pull qwen3:4b-instruct

agents-up: ## Migrate, then run both agents every SCHEDULE_EVERY=2m in shadow mode (needs `make ollama`), traces in Jaeger
	SCHEDULE_EVERY=$(SCHEDULE_EVERY) docker compose --profile agents --profile tracing up -d --build

agents-logs: ## Follow the scheduler and its jobs
	docker compose --profile agents logs -f worker-scheduler

agents-down: ## Stop the scheduler (SIGTERM reaches the running job; data kept)
	docker compose --profile agents stop worker-scheduler

psql: ## Open a psql shell on the local database
	psql "$(DATABASE_URL)"

logs: ## Follow service logs
	docker compose logs -f

migrate: ## Apply pending migrations
	goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: ## Roll back the last migration
	goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-status: ## Show migration status
	goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" status

seed: ## Load seed data (safe to re-run)
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f $(SEED_FILE)

run-support: ## Run one support batch on the dev DB (MODEL=ollama/qwen3:8b|fake/escalate-all, BATCH_SIZE=20, OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14317 traces to Jaeger)
	DATABASE_URL="$(DATABASE_URL)" MODEL="$(MODEL)" $(if $(BATCH_SIZE),BATCH_SIZE="$(BATCH_SIZE)") $(if $(OTEL_EXPORTER_OTLP_ENDPOINT),OTEL_EXPORTER_OTLP_ENDPOINT="$(OTEL_EXPORTER_OTLP_ENDPOINT)") go run ./cmd/worker -agent=support

run-orders: ## Run one orders batch on the dev DB: tasks from run-support become proposals (MODEL, BATCH_SIZE as above)
	DATABASE_URL="$(DATABASE_URL)" MODEL="$(MODEL)" $(if $(BATCH_SIZE),BATCH_SIZE="$(BATCH_SIZE)") $(if $(OTEL_EXPORTER_OTLP_ENDPOINT),OTEL_EXPORTER_OTLP_ENDPOINT="$(OTEL_EXPORTER_OTLP_ENDPOINT)") go run ./cmd/worker -agent=orders

purge-runs: ## Delete run_items (and their steps) older than RETENTION_DAYS=30; see docs/adr/0014
	echo "DELETE FROM run_items WHERE created_at < now() - make_interval(days => :'days'::int);" | psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -v days="$(RETENTION_DAYS)"

# --- Evals (docs/autonomy.md) -------------------------------------------------
# Evals rewrite queue state, so they run on their own database; cmd/eval
# refuses any database whose name does not start with triage_eval.
EVAL_DB           ?= triage_eval
EVAL_DATABASE_URL ?= postgres://triage:triage@127.0.0.1:15432/$(EVAL_DB)?sslmode=disable
AGENT             ?= support
MODEL_SLUG         = $(subst .,-,$(subst :,-,$(subst /,-,$(MODEL))))
CASSETTE          ?= evals/cassettes/$(AGENT)/$(MODEL_SLUG).json
# The committed cassettes the db-check replay gate uses (one per agent), when present.
CI_MODEL          := ollama/qwen3:8b
CI_CASSETTES      := $(wildcard evals/cassettes/*/ollama-qwen3-8b.json)
EVAL_FLAGS         = -agent=$(AGENT) -model=$(MODEL) $(if $(OUT),-out=$(OUT)) $(if $(CASES),-cases=$(CASES)) $(if $(LIMIT),-limit=$(LIMIT)) $(if $(REPEAT),-repeat=$(REPEAT)) $(if $(CHECK),-check)
EVAL_RUN           = EVAL_DATABASE_URL="$(EVAL_DATABASE_URL)" go run ./cmd/eval

eval-db: up ## Recreate the eval database (drop, create, migrate, seed)
	psql "$(DATABASE_URL)" -q -v ON_ERROR_STOP=1 -c 'DROP DATABASE IF EXISTS $(EVAL_DB) WITH (FORCE)' -c 'CREATE DATABASE $(EVAL_DB)'
	goose -dir $(MIGRATIONS_DIR) postgres "$(EVAL_DATABASE_URL)" -no-color up > /dev/null
	psql "$(EVAL_DATABASE_URL)" -q -v ON_ERROR_STOP=1 -f $(SEED_FILE) > /dev/null

eval: eval-db ## Score an agent live (AGENT=support|orders MODEL=ollama/qwen3:8b CASES=HD-2001,HD-2005 LIMIT= REPEAT=3 CHECK=1 OUT=evals/results)
	$(EVAL_RUN) $(EVAL_FLAGS)

eval-record: eval-db ## Record a run into CASSETTE (default evals/cassettes/$(AGENT)/<model>.json); needs the model (Ollama)
	$(EVAL_RUN) $(EVAL_FLAGS) -record=$(CASSETTE)

eval-replay: eval-db ## Replay CASSETTE; must reproduce the committed summary and meet thresholds (UPDATE=1 rewrites the summary)
	$(EVAL_RUN) -agent=$(AGENT) -model=$(MODEL) -replay=$(CASSETTE) -check $(if $(UPDATE),-update)

eval-oracle: eval-db ## Oracle run of both agents (fake/oracle): every metric at its expected value, results in a temp dir
	out=$$(mktemp -d) && for a in support orders; do $(EVAL_RUN) -agent=$$a -model=fake/oracle -concurrency=4 -check -out="$$out" || exit 1; done && cat "$$out"/*/fake-oracle/report.md

eval-compare: ## Markdown table across committed summaries
	go run ./cmd/eval compare evals/results/*/*/summary.json

# Terraform is planned and tested, never applied here (docs/adr/0011). Plugins
# are cached across runs (CI caches the same directories).
TF_DIR := infra/terraform
export TF_PLUGIN_CACHE_DIR ?= $(HOME)/.terraform.d/plugin-cache

tf-check: ## Terraform: fmt, validate, tests (plan on a mock provider), tflint, trivy
	@mkdir -p "$(TF_PLUGIN_CACHE_DIR)"
	terraform fmt -check -recursive $(TF_DIR)
	terraform -chdir=$(TF_DIR) init -backend=false -input=false -no-color > /dev/null
	terraform -chdir=$(TF_DIR) validate -no-color
	terraform -chdir=$(TF_DIR) test -no-color
	tflint --chdir=$(TF_DIR) --init > /dev/null
	tflint --chdir=$(TF_DIR)
	trivy config --quiet --exit-code 1 $(TF_DIR)

check: lint tf-check test db-check ## Everything CI runs

lint: ## Static checks: compose, workflows, shell scripts, Go, vulnerable deps (govulncheck), committed secrets (gitleaks)
	docker compose config --quiet
	actionlint
	shellcheck scripts/*.sh
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@scripts/check-go-version.sh
	go mod tidy -diff
	go vet ./...
	golangci-lint run ./...
	govulncheck ./...
	gitleaks git --no-banner --redact .

test: ## Go unit tests (with the race detector)
	go test -race ./...

db-check: up ## Migrations up/down/up, DB role grants, seed twice, golden labels, DB-backed tests (throwaway DBs)
	psql "$(DATABASE_URL)" -q -v ON_ERROR_STOP=1 -f scripts/drop-stale-test-dbs.sql
	psql "$(DATABASE_URL)" -q -c 'CREATE DATABASE $(CHECK_DB)'
	goose -dir $(MIGRATIONS_DIR) postgres "$(CHECK_DATABASE_URL)" up
	goose -dir $(MIGRATIONS_DIR) postgres "$(CHECK_DATABASE_URL)" down-to 0
	goose -dir $(MIGRATIONS_DIR) postgres "$(CHECK_DATABASE_URL)" up
	psql "$(CHECK_DATABASE_URL)" -q -f scripts/check-db-roles.sql
	$(MAKE) --no-print-directory seed DATABASE_URL="$(CHECK_DATABASE_URL)"
	$(MAKE) --no-print-directory seed DATABASE_URL="$(CHECK_DATABASE_URL)"
	scripts/check-golden.sh "$(CHECK_DATABASE_URL)"
	@# A template must have no connections, so copy it before any test connects.
	psql "$(DATABASE_URL)" -q -c 'CREATE DATABASE $(TEST_TEMPLATE_DB) TEMPLATE $(CHECK_DB)'
	TEST_DATABASE_URL="$(CHECK_DATABASE_URL)" TEST_TEMPLATE_DB=$(TEST_TEMPLATE_DB) go test -race -count=1 ./...
	@# Eval gate, layer 1 is the oracle run above (internal/evals TestOracle_EndToEnd).
	@# Layer 2: each committed cassette must reproduce its committed summary.
	@test -n "$(CI_CASSETTES)" || echo "eval replay gate: skipped, no committed cassette (make eval-record)"
	@for c in $(CI_CASSETTES); do \
	  a=$$(basename $$(dirname $$c)); db=triage_eval_$(CHECK_RUN_ID)_$$a; \
	  psql "$(DATABASE_URL)" -q -c "CREATE DATABASE $$db TEMPLATE $(TEST_TEMPLATE_DB)" || exit 1; \
	  EVAL_DATABASE_URL="postgres://triage:triage@127.0.0.1:15432/$$db?sslmode=disable" \
	    go run ./cmd/eval -agent=$$a -model=$(CI_MODEL) -replay=$$c -check > /dev/null; \
	  status=$$?; psql "$(DATABASE_URL)" -q -c "DROP DATABASE $$db WITH (FORCE)"; \
	  [ $$status -eq 0 ] || exit $$status; echo "eval replay gate: $$c reproduced"; \
	done
	psql "$(DATABASE_URL)" -q -c 'DROP DATABASE $(TEST_TEMPLATE_DB) WITH (FORCE)' -c 'DROP DATABASE $(CHECK_DB) WITH (FORCE)'
