output "commerce_api_uri" {
  description = "IAM-protected URL of the commerce API."
  value       = google_cloud_run_v2_service.commerce_api.uri
}

output "jobs" {
  description = "Cloud Run Jobs: the two workers and purge (scheduled), and migrate (run on deploy)."
  value       = [for j in google_cloud_run_v2_job.app : j.name]
}

output "image_repository" {
  description = "Where CI pushes the app image."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.app.repository_id}"
}

output "deployer_service_account" {
  description = "Service account GitHub Actions impersonates."
  value       = google_service_account.app["deployer"].email
}

output "workload_identity_provider" {
  description = "Value for google-github-actions/auth workload_identity_provider."
  value       = google_iam_workload_identity_pool_provider.github.name
}
