resource "google_artifact_registry_repository" "app" {
  location      = var.region
  repository_id = "support-triage"
  format        = "DOCKER"
  description   = "The app image; deploys reference it by digest."

  docker_config {
    immutable_tags = true
  }

  depends_on = [google_project_service.api]
}
