# One apply wires Latchkey and GCP together: a tenant's federation
# target, its grant to a product, and the GCP workload identity trust
# that takes the target's subject tokens.
#
# There is no cycle. The target only *names* the workload identity
# provider — `workload_identity_provider` is a path string, written here
# from the project's number and two ids before the provider exists. The
# GCP provider resource is the one that depends on the target (its
# condition names the target's ref), and the IAM binding on the target's
# principal set.

terraform {
  required_providers {
    latchkey = {
      source = "latchkeyid/latchkey"
    }
    google = {
      source = "hashicorp/google"
    }
  }
}

provider "latchkey" {
  issuer = "https://auth.latchkey.id"
  org    = "acme"
  # client_id / client_secret via LATCHKEY_CLIENT_ID / LATCHKEY_CLIENT_SECRET
}

provider "google" {
  project = var.project_id
}

variable "project_id" {
  type        = string
  description = "The GCP project the deployer works in."
}

variable "tenant" {
  type        = string
  description = "The Latchkey tenant the target belongs to."
  default     = "shop"
}

data "google_project" "this" {
  project_id = var.project_id
}

locals {
  pool_id     = "latchkey"
  provider_id = "latchkey"

  # the provider's resource name, by the project's number — a string
  # known at plan time, so the target does not wait for the provider
  workload_identity_provider = "projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${local.pool_id}/providers/${local.provider_id}"
}

resource "google_service_account" "deployer" {
  account_id   = "latchkey-deployer"
  display_name = "Deploys on behalf of Latchkey subject tokens"
}

# Latchkey's side: the target, and who may mint for it.
resource "latchkey_federation_target" "deployer" {
  tenant      = var.tenant
  name        = "gcp-deployer"
  cloud       = "gcp"
  label       = "GCP deployer"
  description = "Deploys to ${var.project_id}."

  gcp = {
    workload_identity_provider = local.workload_identity_provider
    service_account            = google_service_account.deployer.email
  }
}

resource "latchkey_federation_target_grant" "runsheet" {
  tenant       = latchkey_federation_target.deployer.tenant
  target       = latchkey_federation_target.deployer.name
  product      = "runsheet"
  purposes     = ["deploy"]
  environments = ["production"]
}

# GCP's side: a pool, a provider that trusts Latchkey's issuer for this
# tenant and this target only, and the service account's binding.
resource "google_iam_workload_identity_pool" "latchkey" {
  workload_identity_pool_id = local.pool_id
  display_name              = "Latchkey"
}

resource "google_iam_workload_identity_pool_provider" "latchkey" {
  workload_identity_pool_id          = google_iam_workload_identity_pool.latchkey.workload_identity_pool_id
  workload_identity_pool_provider_id = local.provider_id

  attribute_mapping = {
    "google.subject"    = "assertion.sub"
    "attribute.target"  = "assertion.target"
    "attribute.tenant"  = "assertion.tenant"
    "attribute.product" = "assertion.product"
  }

  # Latchkey's own recommendation, read back from the target:
  #   assertion.tenant == "<the tenant's ref>" && assertion.target == "<target_ref>"
  # The tenant claim is the tenant's ref, which is not always its slug, so
  # take the condition from the target rather than spelling the tenant
  # here. To admit several targets through one provider, write your own
  # from each target's `target_ref`:
  #   assertion.target in ["${latchkey_federation_target.a.target_ref}", …]
  attribute_condition = latchkey_federation_target.deployer.attribute_condition

  oidc {
    issuer_uri = "https://auth.latchkey.id"
    # no allowed_audiences: the target's default audience is the
    # provider's own canonical name, which GCP accepts as it stands
  }
}

# Every subject token minted for the target may act as the service
# account. The member is principalSet://…/attribute.target/<target_ref>.
resource "google_service_account_iam_member" "deployer" {
  service_account_id = google_service_account.deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = latchkey_federation_target.deployer.principal_set
}

output "target_ref" {
  description = "What the product stores to ask Latchkey for this target's subject tokens."
  value       = latchkey_federation_target.deployer.target_ref
}
