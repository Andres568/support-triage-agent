resource "google_sql_database_instance" "main" {
  name                = "triage-pg"
  database_version    = "POSTGRES_18"
  region              = var.region
  deletion_protection = true

  settings {
    edition                     = "ENTERPRISE"
    tier                        = var.db_tier
    availability_type           = "ZONAL"
    deletion_protection_enabled = true

    # Diagnostics in Cloud Logging (connections, lock waits, temp files,
    # checkpoints); statement logging stays off so no customer text is logged.
    database_flags {
      name  = "log_checkpoints"
      value = "on"
    }
    database_flags {
      name  = "log_connections"
      value = "on"
    }
    database_flags {
      name  = "log_disconnections"
      value = "on"
    }
    database_flags {
      name  = "log_lock_waits"
      value = "on"
    }
    database_flags {
      name  = "log_temp_files"
      value = "0"
    }

    # Backups hold every row, transcripts included, so they bound how long
    # purged data survives: 7 daily backups plus 7 days of WAL for
    # point-in-time recovery (docs/adr/0014).
    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = 7
        retention_unit   = "COUNT"
      }
    }

    # Public IP with no authorized networks: nothing can connect directly.
    # trivy GCP-0017 (public address) is ignored for this block only: a
    # private IP needs a VPC plus Private Service Access, cost and moving
    # parts this demo does not need while no network is authorized.
    # Cloud Run reaches the instance through its built-in Cloud SQL volume
    # (the Auth Proxy, IAM-authorized by roles/cloudsql.client), and TLS is
    # required for everything else.
    #trivy:ignore:GCP-0017
    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
    }
  }

  depends_on = [google_project_service.api]
}

resource "google_sql_database" "triage" {
  name     = "triage"
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "triage" {
  name                = "triage"
  instance            = google_sql_database_instance.main.name
  password_wo         = var.db_password
  password_wo_version = var.db_password_version
}

# Login users for the database roles of migration 00013; the migration
# grants each into its NOLOGIN role when the user exists (so apply before
# the first migrate). Password auth for now; Cloud SQL IAM database auth is
# the next step (docs/adr/0015).
locals {
  db_login_users = toset(["triage_commerce", "triage_worker", "triage_purger"])
}

resource "google_sql_user" "role" {
  for_each = local.db_login_users

  name                = each.key
  instance            = google_sql_database_instance.main.name
  password_wo         = var.db_role_passwords[each.key]
  password_wo_version = var.db_password_version
}
