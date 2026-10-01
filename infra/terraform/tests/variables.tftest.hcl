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

run "valid_inputs_plan" {
  command = plan
}

run "rejects_tag_only_image" {
  command = plan
  variables {
    image = "us-central1-docker.pkg.dev/demo-project/support-triage/app:v1"
  }
  expect_failures = [var.image]
}

run "rejects_latest_secret_version" {
  command = plan
  variables {
    secret_versions = {
      database-url-commerce = "latest"
      database-url-worker   = "1"
      database-url-migrate  = "1"
      database-url-purge    = "1"
      anthropic-api-key     = "1"
    }
  }
  expect_failures = [var.secret_versions]
}

run "rejects_timeout_shorter_than_lease" {
  command = plan
  variables {
    lease_seconds        = 600
    task_timeout_seconds = 300
  }
  expect_failures = [var.task_timeout_seconds]
}

run "rejects_missing_secret_version" {
  command = plan
  variables {
    secret_versions = {
      database-url-commerce = "1"
      database-url-worker   = "1"
      database-url-migrate  = "1"
      anthropic-api-key     = "1"
    }
  }
  expect_failures = [var.secret_versions]
}

run "rejects_extra_secret_version" {
  command = plan
  variables {
    secret_versions = {
      database-url-commerce = "1"
      database-url-worker   = "1"
      database-url-migrate  = "1"
      database-url-purge    = "1"
      anthropic-api-key     = "1"
      openai-api-key        = "1"
    }
  }
  expect_failures = [var.secret_versions]
}

run "provider_key_follows_model" {
  command = plan
  variables {
    model = "gpt-6-luna"
    secret_versions = {
      database-url-commerce = "1"
      database-url-worker   = "1"
      database-url-migrate  = "1"
      database-url-purge    = "1"
      openai-api-key        = "1"
    }
  }

  assert {
    condition     = contains(keys(google_secret_manager_secret.app), "openai-api-key") && !contains(keys(google_secret_manager_secret.app), "anthropic-api-key")
    error_message = "the only API key secret is the configured provider's"
  }
}

run "rejects_local_model_provider" {
  command = plan
  variables {
    model = "ollama/qwen3:8b"
  }
  expect_failures = [var.model]
}

run "rejects_timeout_shorter_than_batch" {
  command = plan
  variables {
    worker_batch_size = 10 # 10 * (180 + 10) = 1900s > 900s
  }
  expect_failures = [var.task_timeout_seconds]
}

run "accepts_batch_split_across_concurrency" {
  command = plan
  variables {
    worker_batch_size  = 10
    worker_concurrency = 3 # ceil(10 / 3) * 190 = 760s <= 900s
  }
}

run "rejects_model_not_in_registry" {
  command = plan
  variables {
    model = "anthropic/claude-haiku-4-5" # a provider prefix, not a registry id: the worker would not start
  }
  expect_failures = [var.model]
}

run "rejects_disabled_cost_cap" {
  command = plan
  variables {
    max_run_cost_micros = 0
  }
  expect_failures = [var.max_run_cost_micros]
}
