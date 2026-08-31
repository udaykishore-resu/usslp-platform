# USSLP — KMS keys.
#
# One key per data domain rather than one key per region.
#
# The reason is blast radius and revocation. A single regional key means
# revoking access to the firmware bucket also revokes access to the event
# stream, and a key policy that grants the OTA service what it needs
# necessarily grants it everything. Four keys, four policies, four independent
# revocations.
#
# Every key is REGIONAL and NOT multi-region. A multi-region key has replicas
# whose material is the same in every region, which would let a caller in
# us-east-1 decrypt an eu-west-1 ciphertext — precisely the thing data residency
# forbids. The cost is that a cross-region restore is impossible without
# re-encrypting, and that is the intended cost.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

variable "name_prefix" {
  description = "Prefix for every alias."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "deletion_window_days" {
  description = "Days before a scheduled key deletion completes. 30 rather than the 7-day minimum: a key deleted in error takes every ciphertext encrypted under it with it, permanently, and 30 days is long enough for somebody to notice a mistake made on a Friday."
  type        = number
  default     = 30

  validation {
    condition     = var.deletion_window_days >= 30
    error_message = "deletion_window_days must be at least 30. A shorter window on a key protecting a statutory audit record is not a saving worth making."
  }
}

variable "enable_key_rotation" {
  description = "Annual automatic rotation. On everywhere; the old material is retained so existing ciphertext stays readable."
  type        = bool
  default     = true
}

variable "administrator_arns" {
  description = "Principals that may administer these keys — a break-glass role, not a person and not a service."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Tags applied to every key."
  type        = map(string)
  default     = {}
}

data "aws_caller_identity" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id

  common_tags = merge(var.tags, {
    ManagedBy         = "terraform"
    "usslp.io/region" = var.region
  })

  # Four domains. The descriptions are the policy: if a use does not fit one of
  # these, it needs its own key rather than borrowing the closest.
  keys = {
    events = {
      description = "USSLP event stream (MSK). Encrypts price changes, device events, telemetry and the statutory audit-log stream at rest."
      service     = "kafka.amazonaws.com"
    }
    database = {
      description = "USSLP relational and cache tiers (Aurora, ElastiCache)."
      service     = "rds.amazonaws.com"
    }
    firmware = {
      description = "USSLP firmware artifacts (S3, object-locked). A firmware image that reaches a label is one the label's bootloader will trust; the key protecting it is the key protecting the fleet."
      service     = "s3.amazonaws.com"
    }
    secrets = {
      description = "USSLP application secrets (Secrets Manager). Price-authority keys, MQTT credentials, POS binding configurations."
      service     = "secretsmanager.amazonaws.com"
    }
  }
}

data "aws_iam_policy_document" "key" {
  for_each = local.keys

  # The account's root principal, so that IAM policies can grant use of the key.
  # Without this statement the key is administrable only through its own policy
  # and an account with no live administrator has an unusable key forever.
  statement {
    sid       = "EnableIAMPolicies"
    effect    = "Allow"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["arn:aws:iam::${local.account_id}:root"]
    }
  }

  dynamic "statement" {
    for_each = length(var.administrator_arns) > 0 ? [1] : []

    content {
      sid    = "KeyAdministration"
      effect = "Allow"

      actions = [
        "kms:Create*",
        "kms:Describe*",
        "kms:Enable*",
        "kms:List*",
        "kms:Put*",
        "kms:Update*",
        "kms:Revoke*",
        "kms:Disable*",
        "kms:Get*",
        "kms:Delete*",
        "kms:TagResource",
        "kms:UntagResource",
        "kms:ScheduleKeyDeletion",
        "kms:CancelKeyDeletion",
      ]

      resources = ["*"]

      principals {
        type        = "AWS"
        identifiers = var.administrator_arns
      }
    }
  }

  # The AWS service that uses the key, via a grant. ViaService restricts it to
  # calls the service makes on the caller's behalf, so a compromised credential
  # cannot use the key directly.
  statement {
    sid    = "AllowServiceUse"
    effect = "Allow"

    actions = [
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:DescribeKey",
      "kms:CreateGrant",
    ]

    resources = ["*"]

    principals {
      type        = "Service"
      identifiers = [each.value.service]
    }

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["${each.value.service}.${var.region}.amazonaws.com"]
    }
  }

  # Residency. Every key refuses any request from outside its own region, which
  # makes the residency claim a property of the key rather than of the
  # configuration around it.
  statement {
    sid       = "DenyOutOfRegion"
    effect    = "Deny"
    actions   = ["kms:*"]
    resources = ["*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }
  }
}

resource "aws_kms_key" "this" {
  for_each = local.keys

  description             = each.value.description
  deletion_window_in_days = var.deletion_window_days
  enable_key_rotation     = var.enable_key_rotation
  # Single-region, deliberately. See the header.
  multi_region = false
  policy       = data.aws_iam_policy_document.key[each.key].json

  tags = merge(local.common_tags, {
    Name              = "${var.name_prefix}-${each.key}"
    "usslp.io/domain" = each.key
  })
}

resource "aws_kms_alias" "this" {
  for_each = local.keys

  name          = "alias/${var.name_prefix}-${each.key}"
  target_key_id = aws_kms_key.this[each.key].key_id
}

output "key_arns" {
  description = "Key ARN per domain: events, database, firmware, secrets."
  value       = { for k, v in aws_kms_key.this : k => v.arn }
}

output "key_ids" {
  description = "Key id per domain."
  value       = { for k, v in aws_kms_key.this : k => v.key_id }
}

output "alias_names" {
  description = "Alias per domain."
  value       = { for k, v in aws_kms_alias.this : k => v.name }
}
