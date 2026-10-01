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

run "sql" {
  command = plan

  assert {
    condition     = google_sql_database_instance.main.database_version == "POSTGRES_18"
    error_message = "Cloud SQL must be PostgreSQL 18 (compose runs postgres:18)"
  }

  assert {
    condition     = google_sql_database_instance.main.settings[0].edition == "ENTERPRISE"
    error_message = "edition must be ENTERPRISE (custom tiers are not available on Enterprise Plus)"
  }

  assert {
    condition     = google_sql_database_instance.main.settings[0].ip_configuration[0].ssl_mode == "ENCRYPTED_ONLY"
    error_message = "connections must be encrypted"
  }

  assert {
    condition     = length(google_sql_database_instance.main.settings[0].ip_configuration[0].authorized_networks) == 0
    error_message = "no authorized networks: Cloud Run connects through the Cloud SQL volume"
  }

  assert {
    condition     = google_sql_database_instance.main.deletion_protection && google_sql_database_instance.main.settings[0].deletion_protection_enabled
    error_message = "the database must be protected from deletion (Terraform and API)"
  }

  assert {
    condition     = google_sql_database_instance.main.settings[0].backup_configuration[0].enabled
    error_message = "backups must be on"
  }

  assert {
    condition = (
      google_sql_database_instance.main.settings[0].backup_configuration[0].transaction_log_retention_days == 7 &&
      google_sql_database_instance.main.settings[0].backup_configuration[0].backup_retention_settings[0].retained_backups == 7
    )
    error_message = "backup and WAL retention are explicit: they bound how long purged transcripts survive (ADR-0014)"
  }

  assert {
    condition     = toset(keys(google_sql_user.role)) == toset(["triage_commerce", "triage_worker", "triage_purger"])
    error_message = "one login user per least-privilege database role (migration 00013)"
  }
}
