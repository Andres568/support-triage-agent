locals {
  apis = toset([
    "artifactregistry.googleapis.com",
    "cloudresourcemanager.googleapis.com", # project IAM bindings
    "cloudscheduler.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "run.googleapis.com",
    "secretmanager.googleapis.com",
    "sqladmin.googleapis.com",
    "sts.googleapis.com",
  ])
}

# Every resource that calls one of these APIs depends_on this, so a first
# apply does not race the enablement.
resource "google_project_service" "api" {
  for_each = local.apis

  service            = each.key
  disable_on_destroy = false
}
