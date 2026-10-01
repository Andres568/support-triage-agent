terraform {
  required_version = ">= 1.16"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.5"
    }
  }

  # Partial config: `terraform init -backend-config=bucket=<state-bucket>`.
  # The checks run `init -backend=false`; nothing here is ever applied by CI
  # (docs/adr/0011-terraform-tested-not-applied.md).
  backend "gcs" {}
}

provider "google" {
  project = var.project_id
  region  = var.region
}
