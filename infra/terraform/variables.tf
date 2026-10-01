variable "project_id" {
  description = "GCP project that hosts everything."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run, Cloud SQL, Artifact Registry and Cloud Scheduler."
  type        = string
  default     = "us-central1"
}

variable "github_repo" {
  description = "owner/name of the GitHub repository whose deploy workflow may deploy (Workload Identity Federation)."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$", var.github_repo))
    error_message = "github_repo must be owner/name."
  }
}

variable "github_repository_id" {
  description = "Numeric id of github_repo (gh api repos/OWNER/NAME --jq .id). Ids survive renames and cannot be re-registered by someone else, unlike names."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.github_repository_id))
    error_message = "github_repository_id must be numeric."
  }
}

variable "github_repository_owner_id" {
  description = "Numeric id of the repository owner (gh api repos/OWNER/NAME --jq .owner.id)."
  type        = string

  validation {
    condition     = can(regex("^[0-9]+$", var.github_repository_owner_id))
    error_message = "github_repository_owner_id must be numeric."
  }
}

variable "github_environment" {
  description = "GitHub environment the deploy job must run in (its protection rules gate deploys). Empty drops that check."
  type        = string
  default     = "prod"
}

variable "image" {
  description = "App image (commerce-api, worker, migrate under /app/), pinned by digest: REGISTRY/REPO/IMAGE@sha256:<64 hex>."
  type        = string

  validation {
    condition     = can(regex("@sha256:[a-f0-9]{64}$", var.image))
    error_message = "image must be pinned by digest (…@sha256:<64 hex>), never by tag."
  }
}

variable "model" {
  description = "Model registry id the worker jobs use (MODEL, exactly as in internal/providers/registry.go), e.g. claude-haiku-4-5-20251001. Its provider (local.model_providers) picks the one API key secret the workers get."
  type        = string

  validation {
    condition     = contains(keys(local.model_providers), var.model)
    error_message = "model must be a cloud model id from internal/providers/registry.go listed in local.model_providers (models.tf); ollama and fake models are local only."
  }
}

variable "secret_versions" {
  description = "Pinned Secret Manager version numbers per secret: the four database URLs plus <provider>-api-key for var.model. Values are added out-of-band (gcloud secrets versions add), so they never enter state."
  type        = map(string)

  validation {
    condition     = alltrue([for v in values(var.secret_versions) : can(regex("^[1-9][0-9]*$", v))])
    error_message = "secret versions must be version numbers; \"latest\" would change what runs without a deploy."
  }

  validation {
    condition = toset(keys(var.secret_versions)) == toset([
      "database-url-commerce", "database-url-worker", "database-url-migrate", "database-url-purge",
      "${lookup(local.model_providers, var.model, "unknown")}-api-key",
    ])
    error_message = "secret_versions needs exactly: database-url-commerce, database-url-worker, database-url-migrate, database-url-purge and <provider of model>-api-key."
  }
}

variable "db_tier" {
  description = "Cloud SQL machine tier (Enterprise edition custom tier)."
  type        = string
  default     = "db-custom-1-3840"
}

variable "db_password" {
  description = "Password of the triage database user (owner of the schema; the migrate job). Write-only: sent to the API, never stored in state or plan."
  type        = string
  sensitive   = true
  ephemeral   = true
}

variable "db_role_passwords" {
  description = "Passwords of the per-role login users triage_commerce, triage_worker and triage_purger (migration 00013 grants them commerce_ro, worker_rw and purger). Write-only, like db_password."
  type        = map(string)
  sensitive   = true
  ephemeral   = true
}

variable "db_password_version" {
  description = "Bump to rotate db_password and db_role_passwords (write-only attributes change only when their version does)."
  type        = number
  default     = 1
}

variable "lease_seconds" {
  description = "Worker claim lease (LEASE); must exceed item_timeout_seconds (validated by the worker at startup)."
  type        = number
  default     = 240
}

variable "worker_batch_size" {
  description = "Items one worker job execution claims (BATCH_SIZE)."
  type        = number
  default     = 4
}

variable "worker_concurrency" {
  description = "Items a worker job processes at once (CONCURRENCY)."
  type        = number
  default     = 1
}

variable "item_timeout_seconds" {
  description = "Per-item deadline, model calls and retries included (ITEM_TIMEOUT)."
  type        = number
  default     = 180
}

variable "finalize_timeout_seconds" {
  description = "Per-item deadline of the fenced finalize write (FINALIZE_TIMEOUT)."
  type        = number
  default     = 10
}

variable "task_timeout_seconds" {
  description = "Cloud Run task timeout for the jobs. It covers a whole batch, so a task is never killed with items in hand: at least ceil(batch / concurrency) * (item + finalize) seconds, and at least one lease."
  type        = number
  default     = 900

  validation {
    condition     = var.task_timeout_seconds >= var.lease_seconds
    error_message = "task_timeout_seconds must be >= lease_seconds."
  }

  validation {
    condition     = var.task_timeout_seconds >= ceil(var.worker_batch_size / var.worker_concurrency) * (var.item_timeout_seconds + var.finalize_timeout_seconds)
    error_message = "task_timeout_seconds must be >= ceil(worker_batch_size / worker_concurrency) * (item_timeout_seconds + finalize_timeout_seconds)."
  }
}

variable "schedule" {
  description = "Cron (UTC) for the worker jobs. Cloud Scheduler does not skip a fire while the previous execution runs; the queue's fencing makes that harmless."
  type        = string
  default     = "*/5 * * * *"
}

variable "retention_days" {
  description = "Days of run_items (transcripts: customer text and order details) the daily purge job keeps (docs/adr/0014)."
  type        = number
  default     = 30

  validation {
    condition     = var.retention_days >= 1 && floor(var.retention_days) == var.retention_days
    error_message = "retention_days must be a whole number >= 1."
  }
}

variable "purge_schedule" {
  description = "Cron (UTC) for the purge job."
  type        = string
  default     = "17 3 * * *"
}

variable "max_run_cost_micros" {
  description = "Per-execution spend cap of a worker job in micro-USD (MAX_RUN_COST_MICROS): the run stops claiming once it has spent this. It bounds one runaway batch, not a day; the daily bound is the provider spend limit and a billing budget (docs/adr/0015)."
  type        = number
  default     = 1000000 # 1 USD per execution

  validation {
    condition     = var.max_run_cost_micros >= 1 && floor(var.max_run_cost_micros) == var.max_run_cost_micros
    error_message = "max_run_cost_micros must be a whole number >= 1; 0 would disable the cap in production."
  }
}
