# One service account per workload, each with only what it uses.
locals {
  service_accounts = {
    "support-worker" = "Support worker job"
    "orders-worker"  = "Orders worker job"
    "commerce-api"   = "Internal read-only commerce API"
    scheduler        = "Cloud Scheduler: runs the scheduled jobs"
    migrate          = "Migration job"
    purge            = "Transcript retention (purge) job"
    deployer         = "GitHub Actions deploys (Workload Identity Federation)"
  }

  # Everything that runs code and connects to Cloud SQL.
  runtime_service_accounts = toset(["support-worker", "orders-worker", "commerce-api", "migrate", "purge"])
  worker_service_accounts  = toset(["support-worker", "orders-worker"])

  # Which secrets each runtime reads, keyed "<sa>/<secret>": exactly the
  # ones mounted by its job or service (run.tf).
  secret_readers = merge(
    { for p in flatten([for j in local.jobs : [for s in values(j.secrets) : { sa = j.sa, secret = s }]]) : "${p.sa}/${p.secret}" => p },
    { "commerce-api/database-url-commerce" = { sa = "commerce-api", secret = "database-url-commerce" } },
  )
  # commerce-api has its own resource below: the jobs' env references
  # commerce-api (its URL), so their grants cannot gate commerce-api too.
  job_secret_readers = { for k, v in local.secret_readers : k => v if v.sa != "commerce-api" }
}

resource "google_service_account" "app" {
  for_each = local.service_accounts

  account_id   = "triage-${each.key}"
  display_name = each.value

  depends_on = [google_project_service.api]
}

# The only project-level grant: connecting through the Cloud SQL volume.
# Everything else is granted on the resource (secret, job, service,
# repository, service account).
resource "google_project_iam_member" "cloudsql_client" {
  for_each = local.runtime_service_accounts

  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.app[each.key].email}"

  depends_on = [google_project_service.api]
}

resource "google_secret_manager_secret_iam_member" "reader" {
  for_each = local.job_secret_readers

  secret_id = google_secret_manager_secret.app[each.value.secret].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.app[each.value.sa].email}"
}

resource "google_secret_manager_secret_iam_member" "commerce_api_reader" {
  secret_id = google_secret_manager_secret.app["database-url-commerce"].id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.app["commerce-api"].email}"
}

# The workers call commerce-api with an ID token; nobody else may invoke it.
resource "google_cloud_run_v2_service_iam_member" "worker_invokes_commerce_api" {
  for_each = local.worker_service_accounts

  name     = google_cloud_run_v2_service.commerce_api.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.app[each.key].email}"
}

# The scheduler can run the scheduled jobs and nothing else.
resource "google_cloud_run_v2_job_iam_member" "scheduler_runs_job" {
  for_each = local.scheduled_jobs

  name     = google_cloud_run_v2_job.app[each.key].name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.app["scheduler"].email}"
}

resource "google_artifact_registry_repository_iam_member" "deployer_pushes" {
  repository = google_artifact_registry_repository.app.name
  location   = var.region
  role       = "roles/artifactregistry.writer"
  member     = "serviceAccount:${google_service_account.app["deployer"].email}"
}

# The deployer rolls new images onto the existing jobs and service (and
# executes migrate); it cannot create or touch any other Run resource.
# Creating them is a human `terraform apply` (docs/adr/0011).
resource "google_cloud_run_v2_job_iam_member" "deployer_develops" {
  for_each = local.jobs

  name     = google_cloud_run_v2_job.app[each.key].name
  location = var.region
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.app["deployer"].email}"
}

resource "google_cloud_run_v2_service_iam_member" "deployer_develops_commerce_api" {
  name     = google_cloud_run_v2_service.commerce_api.name
  location = var.region
  role     = "roles/run.developer"
  member   = "serviceAccount:${google_service_account.app["deployer"].email}"
}

# actAs lets the deployer's revisions run as the runtime SAs.
resource "google_service_account_iam_member" "deployer_acts_as" {
  for_each = local.runtime_service_accounts

  service_account_id = google_service_account.app[each.key].name
  role               = "roles/iam.serviceAccountUser"
  member             = "serviceAccount:${google_service_account.app["deployer"].email}"
}
