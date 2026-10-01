locals {
  cloudsql_mount = "/cloudsql"

  # Plain env shared by the worker jobs. AUTONOMY is pinned to shadow: every
  # decision is recorded, nothing is auto-replied (docs/autonomy.md). The
  # batch knobs are set here, not left to the binary's defaults, because
  # var.task_timeout_seconds is validated against them. No
  # OTEL_EXPORTER_OTLP_ENDPOINT: there is no collector on GCP yet, so the
  # jobs export no traces (docs/adr/0010).
  worker_env = {
    AUTONOMY              = "shadow"
    MODEL                 = var.model
    BATCH_SIZE            = tostring(var.worker_batch_size)
    CONCURRENCY           = tostring(var.worker_concurrency)
    ITEM_TIMEOUT          = "${var.item_timeout_seconds}s"
    FINALIZE_TIMEOUT      = "${var.finalize_timeout_seconds}s"
    LEASE                 = "${var.lease_seconds}s"
    MAX_RUN_COST_MICROS   = tostring(var.max_run_cost_micros)
    COMMERCE_API_URL      = google_cloud_run_v2_service.commerce_api.uri
    COMMERCE_API_AUDIENCE = google_cloud_run_v2_service.commerce_api.uri
    GOOGLE_CLOUD_PROJECT  = var.project_id
  }
  # The worker's database role cannot read orders: order facts come only
  # through commerce-api. Only the configured provider's key is mounted.
  worker_secrets = {
    DATABASE_URL                             = "database-url-worker"
    "${upper(local.model_provider)}_API_KEY" = "${local.model_provider}-api-key"
  }
  project_env = { GOOGLE_CLOUD_PROJECT = var.project_id }

  # Each job runs as its own service account (iam.tf).
  jobs = {
    "support-worker" = { sa = "support-worker", command = ["/app/worker"], args = ["-agent=support"], env = local.worker_env, secrets = local.worker_secrets }
    "orders-worker"  = { sa = "orders-worker", command = ["/app/worker"], args = ["-agent=orders"], env = local.worker_env, secrets = local.worker_secrets }
    "migrate"        = { sa = "migrate", command = ["/app/migrate"], args = ["up"], env = local.project_env, secrets = { DATABASE_URL = "database-url-migrate" } }
    # Same DELETE as `make purge-runs` (docs/adr/0014).
    "purge" = { sa = "purge", command = ["/app/migrate"], args = ["purge", "-days=${var.retention_days}"], env = local.project_env, secrets = { DATABASE_URL = "database-url-purge" } }
  }
  # migrate runs on deploy (gcloud run jobs execute migrate --wait), not on a schedule.
  scheduled_jobs = {
    "support-worker" = var.schedule
    "orders-worker"  = var.schedule
    "purge"          = var.purge_schedule
  }
}

resource "google_cloud_run_v2_service" "commerce_api" {
  name                = "commerce-api"
  location            = var.region
  deletion_protection = false
  # Reachable at its run.app URL, but only with an ID token from a principal
  # holding run.invoker (the worker SAs): there is no allUsers binding.
  ingress = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.app["commerce-api"].email

    # Two small batch workers call it; a cap bounds cost and DB connections.
    scaling {
      max_instance_count = 3
    }

    containers {
      image   = var.image
      command = ["/app/commerce-api"]

      env {
        name  = "GOOGLE_CLOUD_PROJECT"
        value = var.project_id
      }

      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.app["database-url-commerce"].secret_id
            version = var.secret_versions["database-url-commerce"]
          }
        }
      }

      volume_mounts {
        name       = "cloudsql"
        mount_path = local.cloudsql_mount
      }
    }

    volumes {
      name = "cloudsql"
      cloud_sql_instance {
        instances = [google_sql_database_instance.main.connection_name]
      }
    }
  }

  depends_on = [google_project_service.api, google_secret_manager_secret_iam_member.commerce_api_reader]
}

# Each job execution is one batch: claim, process, finalize, exit.
resource "google_cloud_run_v2_job" "app" {
  for_each = local.jobs

  name                = each.key
  location            = var.region
  deletion_protection = false

  template {
    task_count = 1

    template {
      service_account = google_service_account.app[each.value.sa].email
      max_retries     = 1
      timeout         = "${var.task_timeout_seconds}s"

      containers {
        image   = var.image
        command = each.value.command
        args    = each.value.args

        dynamic "env" {
          for_each = each.value.env
          content {
            name  = env.key
            value = env.value
          }
        }

        dynamic "env" {
          for_each = each.value.secrets
          content {
            name = env.key
            value_source {
              secret_key_ref {
                secret  = google_secret_manager_secret.app[env.value].secret_id
                version = var.secret_versions[env.value]
              }
            }
          }
        }

        volume_mounts {
          name       = "cloudsql"
          mount_path = local.cloudsql_mount
        }
      }

      volumes {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [google_sql_database_instance.main.connection_name]
        }
      }
    }
  }

  depends_on = [google_project_service.api, google_secret_manager_secret_iam_member.reader]
}
