# USSLP — us-east-1 (primary region).
#
#   terraform init
#   terraform plan  -var-file=terraform.tfvars
#   terraform apply -var-file=terraform.tfvars
#
# This composition is the same in all three regions; what differs is the
# variables. That is deliberate — a region whose composition differs structurally
# from the primary is a region whose failure modes have never been rehearsed.
#
# The one genuine difference is data residency, and it is expressed in the two
# other regions rather than here: us-east-1 is the default home for tenants with
# no residency requirement.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }

  # State. A backend is not configured here because the bucket, the table and
  # the role are account-specific and this repository knows none of them.
  # Configure it with `terraform init -backend-config=...`, or uncomment and
  # fill in:
  #
  # backend "s3" {
  #   bucket         = "REPLACE-ME-usslp-tfstate"
  #   key            = "prod/us-east-1/terraform.tfstate"
  #   region         = "us-east-1"
  #   dynamodb_table = "REPLACE-ME-usslp-tflock"
  #   encrypt        = true
  # }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.common_tags
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------

variable "region" {
  description = "AWS region."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "dev | staging | prod. Must match global.environment in the Helm values for this cluster."
  type        = string
  default     = "prod"

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be dev, staging or prod."
  }
}

variable "availability_zones" {
  description = "Three AZs. The label-service PDB minimum of 4 out of a 6-pod floor assumes two pods per zone."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

variable "vpc_cidr" {
  description = "VPC CIDR. Distinct per region so that a future transit-gateway attachment — which residency currently forbids — would not need renumbering."
  type        = string
  default     = "10.10.0.0/16"
}

variable "kms_administrator_arns" {
  description = "Break-glass principals that may administer the KMS keys. A role, not a person."
  type        = list(string)
  default     = []
}

variable "api_access_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API. Empty and endpoint_public_access false means the API is private and reached through a VPN or bastion, which is the production posture."
  type        = list(string)
  default     = []
}

locals {
  name_prefix = "usslp-${var.environment}-use1"

  common_tags = {
    Project                = "usslp"
    Environment            = var.environment
    "usslp.io/region"      = var.region
    "usslp.io/residency"   = "none"
    ManagedBy              = "terraform"
    "usslp.io/composition" = "deploy/terraform/regions/us-east-1"
  }

  secrets_path_prefix = "usslp/${var.environment}/${var.region}"
}

# ---------------------------------------------------------------------------
# Composition
#
# The dependency order is: kms -> network -> eks -> {msk, aurora, elasticache,
# s3} -> iam-irsa. Terraform infers all of it from the references below; the
# only explicit depends_on in this file is where a reference alone would not
# express the ordering.
# ---------------------------------------------------------------------------

module "kms" {
  source = "../../modules/kms"

  name_prefix        = local.name_prefix
  region             = var.region
  administrator_arns = var.kms_administrator_arns
  tags               = local.common_tags
}

module "network" {
  source = "../../modules/network"

  name_prefix        = local.name_prefix
  region             = var.region
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones
  # One NAT per AZ in production: a single NAT means one AZ's failure takes
  # egress from all three.
  single_nat_gateway = var.environment != "prod"
  enable_flow_logs   = true
  tags               = local.common_tags
}

module "eks" {
  source = "../../modules/eks"

  name_prefix        = local.name_prefix
  region             = var.region
  vpc_id             = module.network.vpc_id
  private_subnet_ids = module.network.private_subnet_ids
  public_subnet_ids  = module.network.public_subnet_ids
  endpoint_public_access       = length(var.api_access_cidrs) > 0
  endpoint_public_access_cidrs = var.api_access_cidrs
  # Secrets in etcd are envelope-encrypted with the `secrets` key, not a
  # separate one: they hold the same material the secret store holds.
  kms_key_arn = module.kms.key_arns["secrets"]
  tags        = local.common_tags
}

module "msk" {
  source = "../../modules/msk"

  name_prefix                = local.name_prefix
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.data_subnet_ids
  allowed_security_group_ids = [module.eks.node_security_group_id]
  kms_key_arn                = module.kms.key_arns["events"]
  # Six brokers: the catalogue is 5,472 partitions at replication factor 3,
  # which is 16,416 partition replicas — 2,736 per broker, inside MSK's 4,000
  # ceiling with headroom. The module's precondition enforces this.
  broker_count = 6
  tags         = local.common_tags
}

module "aurora" {
  source = "../../modules/aurora"

  name_prefix                = local.name_prefix
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.data_subnet_ids
  allowed_security_group_ids = [module.eks.node_security_group_id]
  kms_key_arn                = module.kms.key_arns["database"]
  # 07:00-08:00 UTC is 02:00-03:00 Eastern: outside the trading day for the
  # region this cluster serves.
  preferred_backup_window      = "07:00-08:00"
  preferred_maintenance_window = "sun:09:00-sun:10:00"
  deletion_protection          = var.environment == "prod"
  tags                         = local.common_tags
}

module "elasticache" {
  source = "../../modules/elasticache"

  name_prefix                = local.name_prefix
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.data_subnet_ids
  allowed_security_group_ids = [module.eks.node_security_group_id]
  kms_key_arn                = module.kms.key_arns["database"]
  tags                       = local.common_tags
}

module "firmware" {
  source = "../../modules/s3-firmware"

  name_prefix = local.name_prefix
  region      = var.region
  kms_key_arn = module.kms.key_arns["firmware"]
  # Only the OTA service reads it. The role is created by the iam-irsa module
  # below, so the bucket policy is applied through the module's variable rather
  # than referenced here — which would be a cycle.
  reader_role_arns = [module.iam.role_arns["ota-service"]]
  tags             = local.common_tags
}

module "iam" {
  source = "../../modules/iam-irsa"

  name_prefix       = local.name_prefix
  region            = var.region
  oidc_provider_arn = module.eks.oidc_provider_arn
  oidc_provider_url = module.eks.oidc_provider_url
  namespace         = "usslp"
  release_name      = "usslp"

  msk_cluster_arn            = module.msk.cluster_arn
  aurora_cluster_resource_id = module.aurora.cluster_resource_id
  kms_key_arns               = module.kms.key_arns
  secrets_path_prefix        = local.secrets_path_prefix

  # The bucket ARN is constructed rather than taken from module.firmware, to
  # break the cycle: the bucket's policy needs the OTA role's ARN, and the OTA
  # role's policy needs the bucket's ARN. The bucket name is deterministic
  # (name_prefix + account id), so constructing it here is exact rather than a
  # guess.
  firmware_bucket_arn = "arn:aws:s3:::${local.name_prefix}-firmware-${data.aws_caller_identity.current.account_id}"

  tags = local.common_tags
}

data "aws_caller_identity" "current" {}

# ---------------------------------------------------------------------------
# Outputs
#
# These are the values that go into the region's Helm values file. They are
# outputs rather than being written into the file directly, because a values
# file in Git that contains a real account id is a values file that has leaked
# an account id.
# ---------------------------------------------------------------------------

output "helm_values" {
  description = "The values that must be set in deploy/helm/usslp/values-prod-use1.yaml, replacing its REPLACE-ME placeholders."

  value = {
    "global.region"                        = var.region
    "global.environment"                   = var.environment
    "externalSecrets.remotePathPrefix"     = local.secrets_path_prefix
    "topicProvisioning.bootstrapServers"   = module.msk.bootstrap_brokers_sasl_iam
    "topicProvisioning.iamRoleArn"         = module.iam.role_arns["topics"]
    "services.label-service.iamRoleArn"    = module.iam.role_arns["label-service"]
    "services.pos-integration-gw.iamRoleArn" = module.iam.role_arns["pos-integration-gw"]
    "services.device-registry.iamRoleArn"  = module.iam.role_arns["device-registry"]
    "services.ota-service.iamRoleArn"      = module.iam.role_arns["ota-service"]
    "services.api-gateway.iamRoleArn"        = module.iam.role_arns["api-gateway"]
    "services.pricing-ai-service.iamRoleArn" = module.iam.role_arns["pricing-ai-service"]
    "services.promotion-service.iamRoleArn"  = module.iam.role_arns["promotion-service"]
    "services.analytics-service.iamRoleArn"  = module.iam.role_arns["analytics-service"]
    "services.mqtt-broker.iamRoleArn"      = module.iam.role_arns["mqtt-broker"]
    "services.kafka-connect.iamRoleArn"    = module.iam.role_arns["kafka-connect"]
  }
}

output "cluster_name" {
  description = "EKS cluster name, for `aws eks update-kubeconfig`."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint. This is the `cluster` value for the ArgoCD ApplicationSet entry."
  value       = module.eks.cluster_endpoint
}

output "partition_budget" {
  description = "The MSK partition load this cluster was sized for."
  value       = module.msk.partition_budget
}

output "firmware_bucket" {
  description = "The firmware bucket, and the Object Lock terms it was created with."

  value = {
    name      = module.firmware.bucket_name
    retention = module.firmware.retention_mode
  }
}

output "external_secrets_role_arn" {
  description = "Annotate the external-secrets service account with this."
  value       = module.iam.external_secrets_role_arn
}
