# USSLP — ap-south-1 (India data localisation).
#
# Structurally identical to eu-west-1: region-local single-region KMS keys, a
# region-local secret store, a data tier with no route to the internet, no
# peering, a region-pinned firmware bucket, and 365-day flow logs as the
# evidence that nothing left.
#
# Two things are genuinely specific to this region:
#
#   1. ap-south-1 has exactly three availability zones. The zone spread
#      constraint therefore has exactly three buckets, and the label-service
#      PDB minimum of 4 out of a 6-pod floor works out to two pods per zone:
#      lose a zone, keep four. There is no fourth AZ to fall back on, so the
#      capacity model has no slack beyond that.
#
#   2. The maintenance and backup windows are shifted for IST. 19:30-20:30 UTC
#      is 01:00-02:00 IST; the us-east-1 window (07:00 UTC) would be half past
#      noon in Mumbai.
#
# The SLOs and the capacity table are unchanged. An Indian retailer gets the
# same three-second price path.

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

  # backend "s3" {
  #   bucket  = "REPLACE-ME-usslp-tfstate-in"
  #   key     = "prod/ap-south-1/terraform.tfstate"
  #   region  = "ap-south-1"
  #   encrypt = true
  # }
  #
  # In-region, for the same reason as eu-west-1: state contains this region's
  # resource identifiers and endpoint names.
}

provider "aws" {
  region = var.region

  default_tags {
    tags = local.common_tags
  }
}

variable "region" {
  description = "AWS region."
  type        = string
  default     = "ap-south-1"

  validation {
    condition     = var.region == "ap-south-1"
    error_message = "This composition is region-pinned. Data localisation is enforced by region-local KMS keys and a region-local secret store."
  }
}

variable "environment" {
  description = "dev | staging | prod."
  type        = string
  default     = "prod"

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be dev, staging or prod."
  }
}

variable "availability_zones" {
  description = "ap-south-1's three AZs. There is no fourth."
  type        = list(string)
  default     = ["ap-south-1a", "ap-south-1b", "ap-south-1c"]

  validation {
    condition     = length(var.availability_zones) == 3
    error_message = "ap-south-1 has exactly three availability zones and the capacity model assumes three."
  }
}

variable "vpc_cidr" {
  description = "VPC CIDR, distinct from the other regions'."
  type        = string
  default     = "10.30.0.0/16"
}

variable "kms_administrator_arns" {
  description = "Break-glass principals for the KMS keys."
  type        = list(string)
  default     = []
}

variable "api_access_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API."
  type        = list(string)
  default     = []
}

locals {
  name_prefix = "usslp-${var.environment}-aps1"

  common_tags = {
    Project              = "usslp"
    Environment          = var.environment
    "usslp.io/region"    = var.region
    "usslp.io/residency" = "in"
    ManagedBy            = "terraform"
  }

  secrets_path_prefix = "usslp/${var.environment}/${var.region}"
}

module "kms" {
  source = "../../modules/kms"

  name_prefix        = local.name_prefix
  region             = var.region
  administrator_arns = var.kms_administrator_arns
  tags               = local.common_tags
}

module "network" {
  source = "../../modules/network"

  name_prefix             = local.name_prefix
  region                  = var.region
  vpc_cidr                = var.vpc_cidr
  availability_zones      = var.availability_zones
  single_nat_gateway      = var.environment != "prod"
  enable_flow_logs        = true
  flow_log_retention_days = 365
  tags                    = local.common_tags
}

module "eks" {
  source = "../../modules/eks"

  name_prefix                  = local.name_prefix
  region                       = var.region
  vpc_id                       = module.network.vpc_id
  private_subnet_ids           = module.network.private_subnet_ids
  public_subnet_ids            = module.network.public_subnet_ids
  endpoint_public_access       = length(var.api_access_cidrs) > 0
  endpoint_public_access_cidrs = var.api_access_cidrs
  kms_key_arn                  = module.kms.key_arns["secrets"]
  tags                         = local.common_tags
}

module "msk" {
  source = "../../modules/msk"

  name_prefix                = local.name_prefix
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.data_subnet_ids
  allowed_security_group_ids = [module.eks.node_security_group_id]
  kms_key_arn                = module.kms.key_arns["events"]
  broker_count               = 6
  tags                       = local.common_tags
}

module "aurora" {
  source = "../../modules/aurora"

  name_prefix                = local.name_prefix
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.data_subnet_ids
  allowed_security_group_ids = [module.eks.node_security_group_id]
  kms_key_arn                = module.kms.key_arns["database"]
  # 19:30-20:30 UTC is 01:00-02:00 IST.
  preferred_backup_window      = "19:30-20:30"
  preferred_maintenance_window = "sun:21:00-sun:22:00"
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

  name_prefix      = local.name_prefix
  region           = var.region
  kms_key_arn      = module.kms.key_arns["firmware"]
  reader_role_arns = [module.iam.role_arns["ota-service"]]
  tags             = local.common_tags
}

module "iam" {
  source = "../../modules/iam-irsa"

  name_prefix                = local.name_prefix
  region                     = var.region
  oidc_provider_arn          = module.eks.oidc_provider_arn
  oidc_provider_url          = module.eks.oidc_provider_url
  msk_cluster_arn            = module.msk.cluster_arn
  aurora_cluster_resource_id = module.aurora.cluster_resource_id
  kms_key_arns               = module.kms.key_arns
  secrets_path_prefix        = local.secrets_path_prefix
  firmware_bucket_arn        = "arn:aws:s3:::${local.name_prefix}-firmware-${data.aws_caller_identity.current.account_id}"
  tags                       = local.common_tags
}

data "aws_caller_identity" "current" {}

output "helm_values" {
  description = "Values for deploy/helm/usslp/values-prod-aps1.yaml."

  value = {
    "global.region"                          = var.region
    "global.environment"                     = var.environment
    "externalSecrets.remotePathPrefix"       = local.secrets_path_prefix
    "externalSecrets.secretStoreRef.name"    = "usslp-ap-south-1-store"
    "topicProvisioning.bootstrapServers"     = module.msk.bootstrap_brokers_sasl_iam
    "topicProvisioning.iamRoleArn"           = module.iam.role_arns["topics"]
    "services.label-service.iamRoleArn"      = module.iam.role_arns["label-service"]
    "services.pos-integration-gw.iamRoleArn" = module.iam.role_arns["pos-integration-gw"]
    "services.device-registry.iamRoleArn"    = module.iam.role_arns["device-registry"]
    "services.ota-service.iamRoleArn"        = module.iam.role_arns["ota-service"]
    "services.api-gateway.iamRoleArn"        = module.iam.role_arns["api-gateway"]
    "services.pricing-ai-service.iamRoleArn" = module.iam.role_arns["pricing-ai-service"]
    "services.promotion-service.iamRoleArn"  = module.iam.role_arns["promotion-service"]
    "services.analytics-service.iamRoleArn"  = module.iam.role_arns["analytics-service"]
    "services.mqtt-broker.iamRoleArn"        = module.iam.role_arns["mqtt-broker"]
    "services.kafka-connect.iamRoleArn"      = module.iam.role_arns["kafka-connect"]
  }
}

output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = module.eks.cluster_endpoint
}

output "residency_controls" {
  description = "The mechanisms that make the India localisation claim structural."

  value = {
    kms_keys_single_region    = "every key is multi_region = false and denies aws:RequestedRegion != ${var.region}"
    secret_store_region_local = local.secrets_path_prefix
    data_subnets_no_egress    = "the data-tier route table has no default route"
    no_vpc_peering            = "no peering connection and no transit gateway attachment"
    firmware_region_pinned    = "no cross-region replication on the firmware bucket"
    flow_log_retention_days   = 365
    availability_zones        = length(var.availability_zones)
  }
}
