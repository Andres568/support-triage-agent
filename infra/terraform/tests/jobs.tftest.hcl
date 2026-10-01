variables {
  project_id                 = "demo-project"
  github_repo                = "acme/support-triage-agent"
  github_repository_id       = "123456"
  github_repository_owner_id = "654321"
  image                      = "us-central1-docker.pkg.dev/demo-project/support-triage/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  model                      = "claude-haiku-4-5-20251001"
  db_password                = "not-a-real-password"
  db_role_passwords = {
    triage_commerce = "not-a-real-password"
    triage_worker   = "not-a-real-password"
    triage_purger   = "not-a-real-password"
  }
  secret_versions = {
    database-url-commerce = "3"
    database-url-worker   = "3"
    database-url-migrate  = "2"
    database-url-purge    = "1"
    anthropic-api-key     = "1"
  }
}

# Plan only, against a mock provider: no credentials, no API calls. Computed
# attributes get these values during plan so references resolve.
mock_provider "google" {
  override_during = plan

  mock_resource "google_service_account" {
    defaults = {
      email = "x@demo-project.iam.gserviceaccount.com"
      name  = "projects/demo-project/serviceAccounts/x@demo-project.iam.gserviceaccount.com"
    }
  }
  mock_resource "google_sql_database_instance" {
    defaults = {
      connection_name = "demo-project:us-central1:triage-pg"
    }
  }
  mock_resource "google_cloud_run_v2_service" {
    defaults = {
      uri = "https://commerce-api-abc123-uc.a.run.app"
    }
  }
  mock_resource "google_secret_manager_secret" {
    defaults = {
      id = "projects/demo-project/secrets/mock"
    }
  }
  mock_resource "google_iam_workload_identity_pool" {
    defaults = {
      name = "projects/123456789/locations/global/workloadIdentityPools/github"
    }
  }
}

run "jobs" {
  command = plan

  assert {
    condition     = alltrue([for j in google_cloud_run_v2_job.app : can(regex("@sha256:[a-f0-9]{64}$", j.template[0].template[0].containers[0].image))])
    error_message = "every job must run the image pinned by digest"
  }

  assert {
    condition = alltrue(flatten([
      for j in google_cloud_run_v2_job.app : [
        for e in j.template[0].template[0].containers[0].env : [
          for v in e.value_source : v.secret_key_ref[0].version != "latest"
        ]
      ]
    ]))
    error_message = "no secret version may be latest"
  }

  assert {
    condition     = alltrue([for j in google_cloud_run_v2_job.app : j.deletion_protection == false])
    error_message = "jobs must be replaceable (deletion_protection = false)"
  }

  assert {
    condition = alltrue([
      for name in ["support-worker", "orders-worker"] :
      [for e in google_cloud_run_v2_job.app[name].template[0].template[0].containers[0].env : e.value if e.name == "AUTONOMY"] == ["shadow"]
    ])
    error_message = "worker jobs must run with AUTONOMY=shadow"
  }

  assert {
    condition     = alltrue([for j in google_cloud_run_v2_job.app : tonumber(trimsuffix(j.template[0].template[0].timeout, "s")) >= var.lease_seconds])
    error_message = "task timeout must be at least one lease"
  }

  assert {
    condition     = alltrue([for j in google_cloud_run_v2_job.app : j.template[0].template[0].max_retries == 1])
    error_message = "jobs retry once; the queue's attempts handle the rest"
  }

  assert {
    condition     = google_cloud_run_v2_job.app["migrate"].template[0].template[0].containers[0].command == tolist(["/app/migrate"])
    error_message = "migrate job must run the migrate binary"
  }

  assert {
    condition     = toset([for e in google_cloud_run_v2_job.app["migrate"].template[0].template[0].containers[0].env : e.name]) == toset(["DATABASE_URL", "GOOGLE_CLOUD_PROJECT"])
    error_message = "migrate gets only DATABASE_URL (and the project id), not the model API key"
  }

  assert {
    condition = (
      google_cloud_run_v2_job.app["purge"].template[0].template[0].containers[0].command == tolist(["/app/migrate"]) &&
      google_cloud_run_v2_job.app["purge"].template[0].template[0].containers[0].args == tolist(["purge", "-days=30"]) &&
      toset([for e in google_cloud_run_v2_job.app["purge"].template[0].template[0].containers[0].env : e.name]) == toset(["DATABASE_URL", "GOOGLE_CLOUD_PROJECT"])
    )
    error_message = "purge runs `migrate purge -days=<retention_days>` with only its DATABASE_URL"
  }

  assert {
    condition = alltrue([
      for name in ["support-worker", "orders-worker"] :
      { for e in google_cloud_run_v2_job.app[name].template[0].template[0].containers[0].env : e.name => e.value_source[0].secret_key_ref[0].secret if length(e.value_source) > 0 } == { DATABASE_URL = "database-url-worker", ANTHROPIC_API_KEY = "anthropic-api-key" }
    ])
    error_message = "workers mount their own database URL and only the configured provider's key"
  }

  assert {
    condition = alltrue([
      for name in ["support-worker", "orders-worker"] :
      { for e in google_cloud_run_v2_job.app[name].template[0].template[0].containers[0].env : e.name => e.value if length(e.value_source) == 0 }["MAX_RUN_COST_MICROS"] == "1000000"
    ])
    error_message = "workers run with a non-zero per-execution cost cap"
  }

  assert {
    condition = alltrue([
      for name in ["support-worker", "orders-worker"] :
      { for e in google_cloud_run_v2_job.app[name].template[0].template[0].containers[0].env : e.name => e.value if contains(["BATCH_SIZE", "CONCURRENCY", "ITEM_TIMEOUT", "FINALIZE_TIMEOUT", "LEASE"], e.name) } == { BATCH_SIZE = "4", CONCURRENCY = "1", ITEM_TIMEOUT = "180s", FINALIZE_TIMEOUT = "10s", LEASE = "240s" }
    ])
    error_message = "the batch knobs the task timeout is validated against are passed to the workers"
  }

  assert {
    condition     = google_cloud_run_v2_job.app["support-worker"].template[0].template[0].service_account == google_service_account.app["support-worker"].email && google_cloud_run_v2_job.app["orders-worker"].template[0].template[0].service_account == google_service_account.app["orders-worker"].email
    error_message = "each worker job runs as its own service account"
  }

  assert {
    condition     = google_cloud_run_v2_service.commerce_api.template[0].scaling[0].max_instance_count == 3
    error_message = "commerce-api scaling is capped"
  }

  assert {
    condition     = [for e in google_cloud_run_v2_service.commerce_api.template[0].containers[0].env : e.value_source[0].secret_key_ref[0].secret if e.name == "DATABASE_URL"] == ["database-url-commerce"]
    error_message = "commerce-api connects as its read-only role"
  }
}
