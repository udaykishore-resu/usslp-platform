# USSLP — firmware artifact storage.
#
# ===========================================================================
# Why this bucket is different from every other bucket
# ===========================================================================
# A firmware image that reaches a smart label is one the label's bootloader will
# trust and execute. A label is a battery-powered device with a 7–10 year life,
# cemented to a shelf rail, in a store that may be on another continent. If a
# bad image bricks it, there is no recovery over the air — somebody drives to
# the store and replaces 40,000 of them by hand.
#
# So this bucket is written once and never changed:
#
#   Object Lock in COMPLIANCE mode. Not GOVERNANCE. Governance mode can be
#   bypassed by a principal holding s3:BypassGovernanceRetention, which means
#   the protection is only as strong as the IAM policy around it. Compliance
#   mode cannot be bypassed by anyone, including the account root, for the
#   length of the retention period. That is a real operational cost — a
#   mistakenly uploaded artifact occupies storage for years — and it is the
#   correct trade for an artifact whose integrity the fleet depends on.
#
#   Versioning is mandatory (Object Lock requires it), so re-uploading under the
#   same key creates a new version rather than replacing the old one. The OTA
#   service records the version id it dispatched; a rollback fetches that exact
#   version, not "whatever is at that key now".
#
# The bucket does NOT hold the signing keys. Artifacts are signed with the
# firmware signing key ring, which lives in Secrets Manager, and the OTA service
# verifies the signature before it accepts an upload at all — with no key
# configured it starts normally and refuses every upload, which it logs once at
# boot. Object Lock protects the artifact from being changed; the signature is
# what proves it was ours in the first place.

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
  description = "Prefix for the bucket name."
  type        = string
}

variable "region" {
  description = "AWS region. The bucket is region-pinned; there is no cross-region replication, because a firmware artifact for an EU fleet has no business in another region."
  type        = string
}

variable "kms_key_arn" {
  description = "KMS key for SSE-KMS. From the kms module's `firmware` domain."
  type        = string
}

variable "retention_years" {
  description = <<-EOT
    Object Lock retention. Defaults to the label's service life: a fleet
    deployed today may still be running this firmware in seven years, and the
    artifact that produced a field failure has to be retrievable for as long as
    a device might still be running it.

    COMPLIANCE mode means this cannot be shortened after the fact, by anyone.
    Choose it deliberately.
  EOT

  type    = number
  default = 10

  validation {
    condition     = var.retention_years >= 7
    error_message = "retention_years must be at least 7: a smart label has a 7-10 year battery life and the firmware it is running must remain retrievable for as long as it might still be running it."
  }
}

variable "log_bucket_name" {
  description = "Existing bucket for server access logs. Empty disables access logging."
  type        = string
  default     = ""
}

variable "reader_role_arns" {
  description = "Roles permitted to read artifacts — normally just the OTA service's IRSA role. One role, not a shared one: the OTA service reads the firmware bucket and the Label Service does not, and a shared role makes that distinction unenforceable."
  type        = list(string)
  default     = []
}

variable "writer_role_arns" {
  description = "Roles permitted to upload artifacts — the release pipeline, not a service."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

data "aws_caller_identity" "current" {}

locals {
  bucket_name = "${var.name_prefix}-firmware-${data.aws_caller_identity.current.account_id}"

  common_tags = merge(var.tags, {
    ManagedBy         = "terraform"
    "usslp.io/region" = var.region
  })
}

resource "aws_s3_bucket" "firmware" {
  bucket = local.bucket_name

  # Object Lock must be enabled at creation. It cannot be turned on afterwards,
  # which is why this is not a variable.
  object_lock_enabled = true

  tags = merge(local.common_tags, {
    Name = local.bucket_name
  })

  lifecycle {
    # A bucket holding immutable firmware with a decade of Object Lock cannot
    # meaningfully be destroyed anyway — S3 refuses while locked objects remain.
    # This makes the refusal happen at plan time with a readable message.
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_object_lock_configuration" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  rule {
    default_retention {
      mode  = "COMPLIANCE"
      years = var.retention_years
    }
  }

  depends_on = [aws_s3_bucket_versioning.firmware]
}

resource "aws_s3_bucket_server_side_encryption_configuration" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }

    # A bucket key. Without it, SSE-KMS makes one KMS call per object operation,
    # and a fleet-wide rollout reading one artifact from 100,000 stores is
    # 100,000 KMS calls and a throttling incident.
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  rule {
    # ACLs disabled entirely. Every access decision is an IAM or bucket policy
    # decision, in one place, reviewable.
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_logging" "firmware" {
  count = var.log_bucket_name != "" ? 1 : 0

  bucket        = aws_s3_bucket.firmware.id
  target_bucket = var.log_bucket_name
  target_prefix = "${local.bucket_name}/"
}

# Lifecycle: transition, never expire. Object Lock would refuse an expiration
# on a locked object anyway; the transition to a colder class is what keeps a
# decade of artifacts affordable.
resource "aws_s3_bucket_lifecycle_configuration" "firmware" {
  bucket = aws_s3_bucket.firmware.id

  rule {
    id     = "tier-old-artifacts"
    status = "Enabled"

    filter {}

    transition {
      # 90 days: past the point where a rollout is still rolling out, but well
      # inside the window where a rollback to this version is plausible.
      days          = 90
      storage_class = "STANDARD_IA"
    }

    transition {
      # A year. Retrieval from Glacier IR is still immediate, which matters:
      # the reason to fetch a two-year-old artifact is that a device in the
      # field is failing on it, and that is not a request that can wait hours.
      days          = 365
      storage_class = "GLACIER_IR"
    }

    # Incomplete multipart uploads. A firmware artifact is tens of megabytes and
    # an interrupted upload leaves parts that are billed and invisible.
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.firmware]
}

data "aws_iam_policy_document" "firmware" {
  # TLS only. An artifact fetched over plaintext HTTP could be substituted in
  # flight; the signature check would catch it, but a control that fails closed
  # earlier is better than one that fails closed later.
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.firmware.arn, "${aws_s3_bucket.firmware.arn}/*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid       = "DenyUnencryptedUploads"
    effect    = "Deny"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.firmware.arn}/*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms"]
    }
  }

  dynamic "statement" {
    for_each = length(var.reader_role_arns) > 0 ? [1] : []

    content {
      sid    = "AllowArtifactRead"
      effect = "Allow"

      actions = [
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
      ]

      resources = [aws_s3_bucket.firmware.arn, "${aws_s3_bucket.firmware.arn}/*"]

      principals {
        type        = "AWS"
        identifiers = var.reader_role_arns
      }
    }
  }

  dynamic "statement" {
    for_each = length(var.writer_role_arns) > 0 ? [1] : []

    content {
      sid    = "AllowArtifactUpload"
      effect = "Allow"

      actions = [
        "s3:PutObject",
        "s3:PutObjectRetention",
        "s3:GetObject",
        "s3:GetObjectVersion",
        "s3:ListBucket",
      ]

      resources = [aws_s3_bucket.firmware.arn, "${aws_s3_bucket.firmware.arn}/*"]

      principals {
        type        = "AWS"
        identifiers = var.writer_role_arns
      }
    }
  }

  # Nobody deletes anything, ever. Object Lock in COMPLIANCE mode already
  # refuses this; the explicit deny makes the intent visible in the policy
  # rather than only in the lock configuration, so that a reviewer sees it.
  statement {
    sid    = "DenyAllDeletes"
    effect = "Deny"

    actions = [
      "s3:DeleteObject",
      "s3:DeleteObjectVersion",
      "s3:PutBucketVersioning",
      "s3:PutObjectLegalHold",
    ]

    resources = ["${aws_s3_bucket.firmware.arn}/*"]

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
  }

  statement {
    sid       = "DenyOutOfRegion"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.firmware.arn, "${aws_s3_bucket.firmware.arn}/*"]

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

resource "aws_s3_bucket_policy" "firmware" {
  bucket = aws_s3_bucket.firmware.id
  policy = data.aws_iam_policy_document.firmware.json

  depends_on = [aws_s3_bucket_public_access_block.firmware]
}

output "bucket_name" {
  description = "Firmware bucket name."
  value       = aws_s3_bucket.firmware.id
}

output "bucket_arn" {
  description = "Firmware bucket ARN, for the OTA service's IRSA policy."
  value       = aws_s3_bucket.firmware.arn
}

output "retention_mode" {
  description = "Object Lock mode and period, for the compliance record."

  value = {
    mode  = "COMPLIANCE"
    years = var.retention_years
    note  = "COMPLIANCE mode cannot be bypassed by any principal, including the account root, for the retention period."
  }
}
