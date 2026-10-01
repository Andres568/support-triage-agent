# Containers only. Versions are added out-of-band
# (`gcloud secrets versions add database-url-worker --data-file=-`), so no
# secret value ever enters Terraform state; the jobs pin version numbers
# (var.secret_versions).
#
# One database URL per database role (migration 00013), so each workload
# connects as a login user that holds only its role's grants:
#   database-url-commerce  triage_commerce -> commerce_ro (read orders, policies)
#   database-url-worker    triage_worker   -> worker_rw   (the queues and transcripts; no orders)
#   database-url-purge     triage_purger   -> purger      (delete old run_items)
#   database-url-migrate   triage          (owner: runs the migrations)
# Only the configured model provider's API key exists.
locals {
  model_provider = local.model_providers[var.model]

  secrets = toset([
    "database-url-commerce",
    "database-url-worker",
    "database-url-migrate",
    "database-url-purge",
    "${local.model_provider}-api-key",
  ])
}

resource "google_secret_manager_secret" "app" {
  for_each = local.secrets

  secret_id = each.key

  replication {
    auto {}
  }

  depends_on = [google_project_service.api]
}
