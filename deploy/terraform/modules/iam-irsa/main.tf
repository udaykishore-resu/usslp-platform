# USSLP — IRSA roles, one per service.
#
# ===========================================================================
# One role per service, never one shared role
# ===========================================================================
# The OTA service reads the firmware bucket. The Label Service does not. The
# Device Registry reads the device PKI secret. Nothing else does. A shared role
# makes every one of those distinctions unenforceable, and the only way to find
# out which service actually needed which permission is to remove one and see
# what breaks.
#
# The trust policy of each role names both the namespace AND the service
# account. Naming only the namespace — `system:serviceaccount:usslp:*` — would
# let any pod in the namespace assume any role, which is the same thing as one
# shared role with more steps.
#
# The roles below map to the ServiceAccounts the Helm chart creates:
# usslp-<service>, which is <release>-<service> with release name `usslp`.

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
  description = "Prefix for every role name, e.g. usslp-prod-use1."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "oidc_provider_arn" {
  description = "EKS OIDC provider ARN, from the eks module."
  type        = string
}

variable "oidc_provider_url" {
  description = "EKS OIDC issuer URL without the scheme, from the eks module."
  type        = string
}

variable "namespace" {
  description = "Kubernetes namespace the service accounts live in."
  type        = string
  default     = "usslp"
}

variable "release_name" {
  description = "Helm release name. The chart names its ServiceAccounts <release>-<service>."
  type        = string
  default     = "usslp"
}

variable "msk_cluster_arn" {
  description = "MSK cluster ARN, for the Kafka IAM policies."
  type        = string
}

variable "firmware_bucket_arn" {
  description = "Firmware bucket ARN. Only the OTA service gets read access to it."
  type        = string
}

variable "aurora_cluster_resource_id" {
  description = "Aurora cluster resource id. IAM database authentication policies are written against this, not the cluster name — the name can be reused, the resource id cannot."
  type        = string
}

variable "kms_key_arns" {
  description = "KMS key ARNs by domain, from the kms module."
  type        = map(string)
}

variable "secrets_path_prefix" {
  description = "Secrets Manager path prefix for this region's secrets. Must match externalSecrets.remotePathPrefix in the Helm values file."
  type        = string
}

variable "tags" {
  description = "Tags applied to every role."
  type        = map(string)
  default     = {}
}

data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  partition  = data.aws_partition.current.partition

  common_tags = merge(var.tags, {
    ManagedBy = "terraform"
  })

  # Kafka topic ARNs are built from the cluster ARN by substituting the resource
  # type. `topic/<cluster-name>/<cluster-uuid>/<topic>` is the shape MSK's IAM
  # authorisation expects.
  msk_cluster_suffix = replace(var.msk_cluster_arn, "/^arn:[^:]*:kafka:[^:]*:[^:]*:cluster\\//", "")
  msk_topic_arn      = replace(var.msk_cluster_arn, ":cluster/", ":topic/")
  msk_group_arn      = replace(var.msk_cluster_arn, ":cluster/", ":group/")

  # The service accounts the chart creates, and what each genuinely needs.
  services = {
    "label-service" = {
      # Produces price-updates and label-state, consumes price-updates and
      # device-events. Reads its price-authority key from Secrets Manager.
      kafka_produce = ["price-updates", "label-state", "label-delivery", "audit-log", "dead-letter"]
      kafka_consume = ["price-updates", "device-events", "promotion-events", "label-state"]
      secrets       = ["label-service/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "pos-integration-gw" = {
      # The front door. Produces pos-integration and price-updates; consumes
      # nothing — it is the top of the path.
      kafka_produce = ["pos-integration", "price-updates", "inventory-sync", "audit-log", "dead-letter"]
      kafka_consume = []
      secrets       = ["uig/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "device-registry" = {
      kafka_produce = ["device-events", "label-telemetry", "audit-log", "dead-letter"]
      kafka_consume = ["device-events", "ota-commands"]
      secrets       = ["device-registry/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "ota-service" = {
      # The only service with firmware bucket access.
      kafka_produce = ["ota-commands", "audit-log", "dead-letter"]
      kafka_consume = ["device-events", "ota-commands"]
      secrets       = ["ota-service/*"]
      kms_domains   = ["events", "secrets", "firmware", "database"]
      database      = true
      firmware_read = true
    }

    "api-gateway" = {
      # The front door. It proxies to the other services over HTTP and does not
      # touch the event stream at all — no Kafka, no database. It reads its JWKS
      # and its TLS material from Secrets Manager and nothing else.
      kafka_produce = []
      kafka_consume = []
      secrets       = ["api-gateway/*"]
      kms_domains   = ["secrets"]
      database      = false
      firmware_read = false
    }

    "pricing-ai-service" = {
      # The workload is named for the capacity table; the binary is
      # platform/cmd/pricing-service and the metric `service` label is
      # "pricing-service". The ServiceAccount the chart creates is named for the
      # workload, which is what the trust policy below matches on.
      kafka_produce = ["price-updates", "audit-log", "dead-letter"]
      kafka_consume = ["price-updates", "inventory-sync", "label-telemetry", "pos-integration"]
      secrets       = ["pricing-ai-service/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "promotion-service" = {
      kafka_produce = ["promotion-events", "audit-log", "dead-letter"]
      kafka_consume = ["promotion-events", "price-updates"]
      secrets       = ["promotion-service/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "analytics-service" = {
      # The widest consume list in the module, and deliberately so: this service
      # projects four streams into the columnar store and computes SLO
      # attainment across them. It produces only its own audit trail and its
      # dead letters — an analytics service that could write to price-updates
      # would be a read model able to rewrite its own source.
      kafka_produce = ["audit-log", "dead-letter"]
      kafka_consume = [
        "label-telemetry", "label-delivery", "price-updates",
        "device-events", "inventory-sync", "promotion-events", "pos-integration",
      ]
      secrets       = ["analytics-service/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "mqtt-broker" = {
      # EMQX. No Kafka, no database, no firmware — it is a broker. It reads its
      # cluster cookie and its listener certificate from Secrets Manager.
      kafka_produce = []
      kafka_consume = []
      secrets       = ["mqtt-broker/*"]
      kms_domains   = ["secrets"]
      database      = false
      firmware_read = false
    }

    "kafka-connect" = {
      # Change capture. Reads and writes broadly across the catalogue, which is
      # what a Connect cluster does, and is the one role here with a wildcard on
      # topics — stated rather than hidden.
      kafka_produce = ["*"]
      kafka_consume = ["*"]
      secrets       = ["kafka-connect/*"]
      kms_domains   = ["events", "secrets", "database"]
      database      = true
      firmware_read = false
    }

    "topics" = {
      # The topic-provisioning Job. Creates topics and nothing else; it does not
      # produce, consume, or read a secret.
      kafka_produce = []
      kafka_consume = []
      secrets       = []
      kms_domains   = ["events"]
      database      = false
      firmware_read = false
    }
  }
}

# ---------------------------------------------------------------------------
# Trust policies
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  for_each = local.services

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }

    # The exact service account, not the namespace. This is the line that makes
    # one-role-per-service mean anything.
    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:${var.namespace}:${var.release_name}-${each.key}"]
    }

    # Without this, a token issued for a different audience would be accepted.
    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "service" {
  for_each = local.services

  name                 = "${var.name_prefix}-${each.key}"
  assume_role_policy   = data.aws_iam_policy_document.assume[each.key].json
  max_session_duration = 3600

  tags = merge(local.common_tags, {
    Name               = "${var.name_prefix}-${each.key}"
    "usslp.io/service" = each.key
  })
}

# ---------------------------------------------------------------------------
# Permissions
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "service" {
  for_each = local.services

  # Kafka cluster connect. Every service that touches the stream needs this;
  # the ones that do not, do not get it.
  dynamic "statement" {
    for_each = (length(each.value.kafka_produce) > 0 || length(each.value.kafka_consume) > 0 || each.key == "topics") ? [1] : []

    content {
      sid    = "KafkaConnect"
      effect = "Allow"

      actions = [
        "kafka-cluster:Connect",
        "kafka-cluster:DescribeCluster",
        "kafka-cluster:DescribeClusterDynamicConfiguration",
      ]

      resources = [var.msk_cluster_arn]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.kafka_produce) > 0 ? [1] : []

    content {
      sid    = "KafkaProduce"
      effect = "Allow"

      actions = [
        "kafka-cluster:WriteData",
        "kafka-cluster:DescribeTopic",
        "kafka-cluster:DescribeTopicDynamicConfiguration",
      ]

      resources = [
        for topic in each.value.kafka_produce :
        "${local.msk_topic_arn}/${topic}"
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.kafka_consume) > 0 ? [1] : []

    content {
      sid    = "KafkaConsume"
      effect = "Allow"

      actions = [
        "kafka-cluster:ReadData",
        "kafka-cluster:DescribeTopic",
      ]

      resources = [
        for topic in each.value.kafka_consume :
        "${local.msk_topic_arn}/${topic}"
      ]
    }
  }

  # Consumer groups. Scoped to a prefix per service so that one service cannot
  # join — and therefore rebalance, and therefore stall — another's group.
  dynamic "statement" {
    for_each = length(each.value.kafka_consume) > 0 ? [1] : []

    content {
      sid    = "KafkaConsumerGroup"
      effect = "Allow"

      actions = [
        "kafka-cluster:AlterGroup",
        "kafka-cluster:DescribeGroup",
      ]

      resources = ["${local.msk_group_arn}/${each.key}-*"]
    }
  }

  # Topic creation, for the provisioning Job only.
  dynamic "statement" {
    for_each = each.key == "topics" ? [1] : []

    content {
      sid    = "KafkaTopicAdmin"
      effect = "Allow"

      actions = [
        "kafka-cluster:CreateTopic",
        "kafka-cluster:DescribeTopic",
        "kafka-cluster:AlterTopicDynamicConfiguration",
        "kafka-cluster:DescribeTopicDynamicConfiguration",
      ]

      resources = ["${local.msk_topic_arn}/*"]
    }
  }

  # Explicitly denied, even to the Job that creates topics. Deleting a topic in
  # this platform destroys a compliance record or a compacted read-model source,
  # and it is not an operation any automated process should be able to perform.
  statement {
    sid       = "DenyKafkaTopicDeletion"
    effect    = "Deny"
    actions   = ["kafka-cluster:DeleteTopic"]
    resources = ["${local.msk_topic_arn}/*"]
  }

  dynamic "statement" {
    for_each = length(each.value.secrets) > 0 ? [1] : []

    content {
      sid    = "SecretsRead"
      effect = "Allow"

      actions = [
        "secretsmanager:GetSecretValue",
        "secretsmanager:DescribeSecret",
      ]

      resources = [
        for path in each.value.secrets :
        "arn:${local.partition}:secretsmanager:${var.region}:${local.account_id}:secret:${var.secrets_path_prefix}/${path}"
      ]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.kms_domains) > 0 ? [1] : []

    content {
      sid    = "KMSUse"
      effect = "Allow"

      actions = [
        "kms:Decrypt",
        "kms:GenerateDataKey",
        "kms:DescribeKey",
      ]

      resources = [
        for domain in each.value.kms_domains :
        var.kms_key_arns[domain]
      ]
    }
  }

  dynamic "statement" {
    for_each = each.value.database ? [1] : []

    content {
      sid       = "AuroraIAMAuth"
      effect    = "Allow"
      actions   = ["rds-db:connect"]

      # The database user is named for the service, so a service can only
      # connect as itself. Written against the cluster's resource id rather
      # than its name: a name can be reused after a delete, a resource id
      # cannot.
      resources = [
        "arn:${local.partition}:rds-db:${var.region}:${local.account_id}:dbuser:${var.aurora_cluster_resource_id}/usslp_${replace(each.key, "-", "_")}"
      ]
    }
  }

  dynamic "statement" {
    for_each = each.value.firmware_read ? [1] : []

    content {
      sid    = "FirmwareRead"
      effect = "Allow"

      actions = [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
      ]

      resources = [var.firmware_bucket_arn, "${var.firmware_bucket_arn}/*"]
    }
  }

  # Residency. Every role refuses any request outside its own region, so a
  # misconfigured endpoint in a service running in eu-west-1 fails rather than
  # succeeding against us-east-1.
  statement {
    sid       = "DenyOutOfRegion"
    effect    = "Deny"
    actions   = ["*"]
    resources = ["*"]

    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }

    # IAM and STS are global endpoints and would be denied by the condition
    # above, which would break the role assumption itself.
    condition {
      test     = "ForAllValues:StringNotEquals"
      variable = "aws:CalledVia"
      values   = ["iam.amazonaws.com", "sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "service" {
  for_each = local.services

  name        = "${var.name_prefix}-${each.key}"
  description = "USSLP ${each.key} — least privilege for exactly what this service does"
  policy      = data.aws_iam_policy_document.service[each.key].json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "service" {
  for_each = local.services

  role       = aws_iam_role.service[each.key].name
  policy_arn = aws_iam_policy.service[each.key].arn
}

# ---------------------------------------------------------------------------
# The External Secrets Operator's own role
#
# It reads every secret under the region's prefix, because that is its job. It
# is the one broad grant in this module, and it is why the operator runs in its
# own namespace with its own service account rather than in `usslp`.
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "external_secrets_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:sub"
      values   = ["system:serviceaccount:external-secrets:external-secrets"]
    }

    condition {
      test     = "StringEquals"
      variable = "${var.oidc_provider_url}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "external_secrets" {
  statement {
    sid    = "ReadRegionSecrets"
    effect = "Allow"

    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:DescribeSecret",
      "secretsmanager:ListSecretVersionIds",
    ]

    resources = [
      "arn:${local.partition}:secretsmanager:${var.region}:${local.account_id}:secret:${var.secrets_path_prefix}/*"
    ]
  }

  statement {
    sid       = "DecryptSecrets"
    effect    = "Allow"
    actions   = ["kms:Decrypt", "kms:DescribeKey"]
    resources = [var.kms_key_arns["secrets"]]
  }

  statement {
    sid       = "DenyOutOfRegion"
    effect    = "Deny"
    actions   = ["*"]
    resources = ["*"]

    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestedRegion"
      values   = [var.region]
    }
  }
}

resource "aws_iam_role" "external_secrets" {
  name               = "${var.name_prefix}-external-secrets"
  assume_role_policy = data.aws_iam_policy_document.external_secrets_assume.json

  tags = local.common_tags
}

resource "aws_iam_policy" "external_secrets" {
  name        = "${var.name_prefix}-external-secrets"
  description = "External Secrets Operator — read this region's secrets"
  policy      = data.aws_iam_policy_document.external_secrets.json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "external_secrets" {
  role       = aws_iam_role.external_secrets.name
  policy_arn = aws_iam_policy.external_secrets.arn
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "role_arns" {
  description = "Role ARN per service. These go into the per-service iamRoleArn values in the Helm environment file."
  value       = { for k, v in aws_iam_role.service : k => v.arn }
}

output "external_secrets_role_arn" {
  description = "The External Secrets Operator's role ARN."
  value       = aws_iam_role.external_secrets.arn
}

output "helm_values_snippet" {
  description = "The services block for the region's Helm values file, so the ARNs are copied rather than retyped."

  value = join("\n", concat(
    ["services:"],
    [for k, v in aws_iam_role.service : "  ${k}:\n    iamRoleArn: \"${v.arn}\""],
  ))
}
