# GitHub Actions deploys without a key: its OIDC token is exchanged for the
# deployer SA, and only for the deploy workflow on main of this repository.
resource "google_iam_workload_identity_pool" "github" {
  workload_identity_pool_id = "github"
  display_name              = "GitHub Actions"

  depends_on = [google_project_service.api]
}

locals {
  deploy_workflow_ref = "${var.github_repo}/.github/workflows/deploy.yml@refs/heads/main"

  # Numeric ids, not names: a deleted or renamed repository's name can be
  # re-registered by someone else, its id cannot. job_workflow_ref pins the
  # workflow file, so another workflow in the repo (or a PR branch) cannot
  # mint deploy credentials.
  wif_conditions = concat(
    [
      "assertion.repository_id == \"${var.github_repository_id}\"",
      "assertion.repository_owner_id == \"${var.github_repository_owner_id}\"",
      "assertion.ref == \"refs/heads/main\"",
      "assertion.job_workflow_ref == \"${local.deploy_workflow_ref}\"",
    ],
    var.github_environment == "" ? [] : ["assertion.environment == \"${var.github_environment}\""],
  )
}

resource "google_iam_workload_identity_pool_provider" "github" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.github.workload_identity_pool_id
  workload_identity_pool_provider_id = "github-actions"
  display_name                       = "GitHub Actions OIDC"

  attribute_mapping = {
    "google.subject"                = "assertion.sub"
    "attribute.repository"          = "assertion.repository"
    "attribute.repository_id"       = "assertion.repository_id"
    "attribute.repository_owner_id" = "assertion.repository_owner_id"
    "attribute.ref"                 = "assertion.ref"
    "attribute.job_workflow_ref"    = "assertion.job_workflow_ref"
  }
  # Without a condition any GitHub repository could present a token.
  attribute_condition = join(" && ", local.wif_conditions)

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account_iam_member" "github_deploys_as_deployer" {
  service_account_id = google_service_account.app["deployer"].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github.name}/attribute.repository_id/${var.github_repository_id}"
}
