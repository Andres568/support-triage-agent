# Cloud Scheduler calls the Run Admin API's jobs.run for each scheduled job.
# It does not skip a fire while the previous execution still runs; the
# queue's claim fencing makes an overlap harmless (docs/adr/0010), and the
# purge DELETE is idempotent.
resource "google_cloud_scheduler_job" "run_job" {
  for_each = local.scheduled_jobs

  name      = "run-${each.key}"
  region    = var.region
  schedule  = each.value
  time_zone = "Etc/UTC"

  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.region}/jobs/${google_cloud_run_v2_job.app[each.key].name}:run"

    oauth_token {
      service_account_email = google_service_account.app["scheduler"].email
      scope                 = "https://www.googleapis.com/auth/cloud-platform"
    }
  }

  depends_on = [google_project_service.api, google_cloud_run_v2_job_iam_member.scheduler_runs_job]
}
