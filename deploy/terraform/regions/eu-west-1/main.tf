# USSLP — eu-west-1 (EU data residency).
#
# Structurally identical to us-east-1. What differs:
#
#   1. Every KMS key is region-local and single-region, and each carries a Deny
#      on aws:RequestedRegion != eu-west-1. A ciphertext produced here cannot be
#      decrypted from another region, because the key material does not exist
#      there. That is a stronger claim than a policy: it survives a policy
#      mistake.
#
#   2. The secrets path prefix is region-local, and the External Secrets
#      Operator role can read only under it. A misconfigured service in another
#      region cannot authenticate here even if the network allowed it.
#
#   3. There is no VPC peering and no transit gateway. The network module
#      creates neither, in any region, for exactly this reason: a peering
#      connection is a route by which data could leave.
#
#   4. The firmware bucket is region-pinned with no cross-region replication.
#      An EU fleet's firmware has no business in another region.
#
# What does NOT differ: the SLOs, the capacity table, the alert thresholds. A
# European retailer gets the same three-second price path.

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
  #   bucket = "REPLACE-ME-usslp-tfstate-eu"
  #   key    = "prod/eu-west-1/terraform.tfstate"
  #   region = "eu-west-1"
  #   encrypt = true
  # }
  #
  # NOTE: the state bucket is in eu-west-1, not the primary region's. Terraform
  # state for an EU region contains EU resource identifiers and endpoint names,
  # and storing it in a us-east-1 bucket would be the residency leak nobody
  # thinks to look for.
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
  default     = "eu-west-1"

  validation {
    condition     = var.region == "eu-west-1"
    error_message = "This composition is region-pinned. Data residency is enforced by region-local KMS keys and a region-local secret store; pointing it at another region would produce a stack that looks EU-resident and is not."
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
  description = "Three AZs."
  type        = list(string)
  default     = ["eu-west-1a", "eu-west-1b", "eu-west-1c"]
}

variable "vpc_cidr" {
  description = "VPC CIDR, distinct from the other regions'."
  type        = string
  default     = "10.20.0.0/16"
}

variable "kms_administrator_arns" {
  description = "Break-glass principals for the KMS keys. These must be EU-resident roles."
  type        = list(string)
  default     = []
}

variable "api_access_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API."
  type        = list(string)
  default     = []
}

locals {
  name_prefix = "usslp-${var.environment}-euw1"

  common_tags = {
    Project              = "usslp"
    Environment          = var.environment
    "usslp.io/region"    = var.region
    "usslp.io/residency" = "eu"
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

  name_prefix        = local.name_prefix
  region             = var.region
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones
  single_nat_gateway = var.environment != "prod"
  # Flow logs are not optional in a residency-enforced region: they are the
  # evidence that no traffic left, and they cannot be enabled retroactively for
  # a period that has already passed.
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
  # 01:00-02:00 UTC is the small hours in western Europe, which is what this
  # region serves. The us-east-1 window would be the middle of the trading day.
  preferred_backup_window      = "01:00-02:00"
  preferred_maintenance_window = "sun:03:00-sun:04:00"
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
  description = "Values for deploy/helm/usslp/values-prod-euw1.yaml."

  value = {
    "global.region"                          = var.region
    "global.environment"                     = var.environment
    "externalSecrets.remotePathPrefix"       = local.secrets_path_prefix
    "externalSecrets.secretStoreRef.name"    = "usslp-eu-west-1-store"
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
  description = "The mechanisms that make the EU residency claim structural rather than intended. Useful as evidence in an audit."

  value = {
    kms_keys_single_region    = "every key is multi_region = false and denies aws:RequestedRegion != ${var.region}"
    secret_store_region_local = local.secrets_path_prefix
    data_subnets_no_egress    = "the data-tier route table has no default route; MSK, Aurora and ElastiCache have no path to the internet"
    no_vpc_peering            = "the network module creates no peering connection and no transit gateway attachment"
    firmware_region_pinned    = "no cross-region replication on the firmware bucket"
    flow_log_retention_days   = 365
  }
}
