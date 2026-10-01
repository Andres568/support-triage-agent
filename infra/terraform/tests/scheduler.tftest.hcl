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

run "scheduler" {
  command = plan

  assert {
    condition     = google_cloud_scheduler_job.run_job["support-worker"].http_target[0].uri == "https://run.googleapis.com/v2/projects/demo-project/locations/us-central1/jobs/support-worker:run"
    error_message = "scheduler must call the Run Admin API jobs.run of its job"
  }

  assert {
    condition     = alltrue([for j in google_cloud_scheduler_job.run_job : j.http_target[0].http_method == "POST"])
    error_message = "jobs.run is a POST"
  }

  assert {
    condition     = alltrue([for j in google_cloud_scheduler_job.run_job : j.http_target[0].oauth_token[0].scope == "https://www.googleapis.com/auth/cloud-platform"])
    error_message = "Google APIs take an OAuth access token (not OIDC) with the cloud-platform scope"
  }

  assert {
    condition     = toset(keys(google_cloud_scheduler_job.run_job)) == toset(["support-worker", "orders-worker", "purge"])
    error_message = "the workers and purge are scheduled; migrate runs on deploy"
  }

  assert {
    condition     = google_cloud_scheduler_job.run_job["purge"].schedule == "17 3 * * *" && google_cloud_scheduler_job.run_job["support-worker"].schedule == "*/5 * * * *"
    error_message = "purge runs daily, the workers every five minutes"
  }
}
