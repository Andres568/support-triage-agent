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

# Distinct emails for the SAs whose identity the assertions check (the mock
# default gives every SA the same one).
override_resource {
  target          = google_service_account.app["support-worker"]
  override_during = plan
  values = {
    email = "triage-support-worker@demo-project.iam.gserviceaccount.com"
    name  = "projects/demo-project/serviceAccounts/triage-support-worker@demo-project.iam.gserviceaccount.com"
  }
}
override_resource {
  target          = google_service_account.app["orders-worker"]
  override_during = plan
  values = {
    email = "triage-orders-worker@demo-project.iam.gserviceaccount.com"
    name  = "projects/demo-project/serviceAccounts/triage-orders-worker@demo-project.iam.gserviceaccount.com"
  }
}

run "iam" {
  command = plan

  assert {
    condition     = toset(keys(google_project_iam_member.cloudsql_client)) == toset(["support-worker", "orders-worker", "commerce-api", "migrate", "purge"]) && alltrue([for m in google_project_iam_member.cloudsql_client : m.role == "roles/cloudsql.client"])
    error_message = "the only project-level grant is cloudsql.client, to the runtimes (not the scheduler or deployer)"
  }

  assert {
    condition     = toset([for m in google_cloud_run_v2_service_iam_member.worker_invokes_commerce_api : m.member]) == toset(["serviceAccount:triage-support-worker@demo-project.iam.gserviceaccount.com", "serviceAccount:triage-orders-worker@demo-project.iam.gserviceaccount.com"])
    error_message = "exactly the two worker SAs may invoke commerce-api"
  }

  assert {
    condition     = alltrue([for m in google_cloud_run_v2_service_iam_member.worker_invokes_commerce_api : m.role == "roles/run.invoker"])
    error_message = "commerce-api grants are run.invoker"
  }

  assert {
    condition     = toset(keys(google_cloud_run_v2_job_iam_member.scheduler_runs_job)) == toset(["support-worker", "orders-worker", "purge"])
    error_message = "the scheduler may invoke exactly the two worker jobs and purge"
  }

  assert {
    condition     = alltrue([for m in google_cloud_run_v2_job_iam_member.scheduler_runs_job : m.role == "roles/run.invoker"])
    error_message = "scheduler job grants are run.invoker"
  }

  assert {
    condition = alltrue([
      for sa in ["support-worker", "orders-worker"] :
      toset([for k, m in local.secret_readers : m.secret if m.sa == sa]) == toset(["database-url-worker", "anthropic-api-key"])
    ])
    error_message = "each worker reads only its database URL and the configured provider's key"
  }

  assert {
    condition     = toset([for k, m in local.secret_readers : m.secret if m.sa == "migrate"]) == toset(["database-url-migrate"])
    error_message = "migrate reads only database-url-migrate"
  }

  assert {
    condition     = toset([for k, m in local.secret_readers : m.secret if m.sa == "purge"]) == toset(["database-url-purge"])
    error_message = "purge reads only database-url-purge"
  }

  assert {
    condition     = toset([for k, m in local.secret_readers : m.secret if m.sa == "commerce-api"]) == toset(["database-url-commerce"])
    error_message = "commerce-api reads only database-url-commerce"
  }

  assert {
    condition     = toset(keys(google_secret_manager_secret.app)) == toset(["database-url-commerce", "database-url-worker", "database-url-migrate", "database-url-purge", "anthropic-api-key"])
    error_message = "one database URL per role, and only the configured provider's API key"
  }

  assert {
    condition = alltrue([
      for c in [
        "assertion.repository_id == \"123456\"",
        "assertion.repository_owner_id == \"654321\"",
        "assertion.ref == \"refs/heads/main\"",
        "assertion.job_workflow_ref == \"acme/support-triage-agent/.github/workflows/deploy.yml@refs/heads/main\"",
        "assertion.environment == \"prod\"",
      ] : strcontains(google_iam_workload_identity_pool_provider.github.attribute_condition, c)
    ])
    error_message = "WIF must pin repository id, owner id, main, the deploy workflow and the prod environment"
  }

  assert {
    condition     = endswith(google_service_account_iam_member.github_deploys_as_deployer.member, "/attribute.repository_id/123456")
    error_message = "the deployer binding is on the repository id, not its name"
  }

  assert {
    condition     = toset(keys(google_cloud_run_v2_job_iam_member.deployer_develops)) == toset(["support-worker", "orders-worker", "migrate", "purge"]) && alltrue([for m in google_cloud_run_v2_job_iam_member.deployer_develops : m.role == "roles/run.developer"])
    error_message = "the deployer's run.developer is granted per job, not on the project"
  }

  # Best effort: this lists the bindings this module declares today. trivy
  # (make tf-check) also flags public IAM members on any resource.
  assert {
    condition = alltrue([
      for m in concat(
        [for r in google_project_iam_member.cloudsql_client : r.member],
        [for r in google_secret_manager_secret_iam_member.reader : r.member],
        [for r in google_cloud_run_v2_job_iam_member.scheduler_runs_job : r.member],
        [for r in google_cloud_run_v2_job_iam_member.deployer_develops : r.member],
        [for r in google_service_account_iam_member.deployer_acts_as : r.member],
        [for r in google_cloud_run_v2_service_iam_member.worker_invokes_commerce_api : r.member],
        [google_cloud_run_v2_service_iam_member.deployer_develops_commerce_api.member],
        [google_secret_manager_secret_iam_member.commerce_api_reader.member],
        [google_artifact_registry_repository_iam_member.deployer_pushes.member],
        [google_service_account_iam_member.github_deploys_as_deployer.member],
      ) : !contains(["allUsers", "allAuthenticatedUsers"], m)
    ])
    error_message = "no public (allUsers/allAuthenticatedUsers) binding anywhere"
  }
}

run "wif_without_environment" {
  command = plan
  variables {
    github_environment = ""
  }

  assert {
    condition     = !strcontains(google_iam_workload_identity_pool_provider.github.attribute_condition, "environment") && strcontains(google_iam_workload_identity_pool_provider.github.attribute_condition, "assertion.ref == \"refs/heads/main\"")
    error_message = "an empty github_environment drops only the environment check"
  }
}
