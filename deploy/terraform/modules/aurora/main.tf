# USSLP — Aurora PostgreSQL.
#
# ===========================================================================
# Read this before assuming the platform uses it
# ===========================================================================
# Nothing in the Go tree connects to PostgreSQL today. The event store
# (platform/pkg/eventstore) and every read model are built on
# platform/pkg/kvstore, an embedded LSM store, and the services persist through
# repository interfaces that the Postgres adapter has not yet been written
# against.
#
# This module provisions the cluster that adapter is the documented production
# port to. Everything below — the parameter group, the IAM authentication, the
# backup window, the Performance Insights retention — is chosen for what the
# price path will need, and it is honest about being ahead of the code.
#
# ===========================================================================
# Aurora rather than RDS
# ===========================================================================
# One reason, and it is the price path: Aurora's storage layer replicates six
# ways across three AZs and a writer failover is typically under 30 seconds,
# where an RDS Multi-AZ failover is 60–120. The price path's budget is 3 seconds
# end to end. Neither number fits inside it, so both are an outage — but a
# 30-second outage is a burst of POS retries and a 120-second one is a store
# manager on the phone.

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
  description = "Prefix for every resource name."
  type        = string
}

variable "vpc_id" {
  description = "VPC to place the cluster in."
  type        = string
}

variable "subnet_ids" {
  description = "Data-tier subnet ids, one per AZ. No route to the internet."
  type        = list(string)
}

variable "allowed_security_group_ids" {
  description = "Security groups permitted to reach 5432."
  type        = list(string)
  default     = []
}

variable "engine_version" {
  description = "Aurora PostgreSQL engine version. 16.x to match the compose prod-like profile, so a schema that works locally works here."
  type        = string
  default     = "16.4"
}

variable "instance_class" {
  description = "Writer and reader instance class."
  type        = string
  default     = "db.r6g.2xlarge"
}

variable "reader_count" {
  description = "Read replicas in addition to the writer. Two, one per remaining AZ, so that losing an AZ leaves both a writer and a reader."
  type        = number
  default     = 2
}

variable "kms_key_arn" {
  description = "KMS key for storage encryption. From the kms module's `database` domain."
  type        = string
}

variable "master_username" {
  description = "Master username. The password is generated and stored in Secrets Manager; it is never in Terraform state as plaintext and never in this repository."
  type        = string
  default     = "usslp_admin"
}

variable "backup_retention_days" {
  description = "Automated backup retention."
  type        = number
  default     = 30
}

variable "preferred_backup_window" {
  description = "UTC backup window. Chosen per region to sit outside the local trading day; the default is the small hours in the Americas."
  type        = string
  default     = "07:00-08:00"
}

variable "preferred_maintenance_window" {
  description = "UTC maintenance window, after the backup window."
  type        = string
  default     = "sun:09:00-sun:10:00"
}

variable "deletion_protection" {
  description = "Refuse to delete the cluster. On in every environment: the cost of the extra step is a minute, and the cost of not having it is a region's event store."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

locals {
  common_tags = merge(var.tags, {
    ManagedBy = "terraform"
  })
}

resource "aws_db_subnet_group" "this" {
  name_prefix = "${var.name_prefix}-aurora-"
  subnet_ids  = var.subnet_ids
  description = "USSLP Aurora — data tier, no internet route"

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-aurora"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "aurora" {
  name_prefix = "${var.name_prefix}-aurora-"
  description = "USSLP Aurora PostgreSQL"
  vpc_id      = var.vpc_id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-aurora"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "aurora" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.aurora.id
  referenced_security_group_id = each.value
  from_port                    = 5432
  to_port                      = 5432
  ip_protocol                  = "tcp"
  description                  = "PostgreSQL from the cluster"
}

# ---------------------------------------------------------------------------
# Parameter groups
# ---------------------------------------------------------------------------

resource "aws_rds_cluster_parameter_group" "this" {
  name_prefix = "${var.name_prefix}-aurora-"
  family      = "aurora-postgresql16"
  description = "USSLP Aurora PostgreSQL cluster parameters"

  # Change capture into the event stream. The outbox pattern the Postgres
  # adapter will use needs logical replication, and turning it on requires a
  # reboot — so it is on from the start rather than discovered later.
  parameter {
    name         = "rds.logical_replication"
    value        = "1"
    apply_method = "pending-reboot"
  }

  # TLS is not optional. The price path carries a statutory audit record.
  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  # Log anything slower than 500 ms. That is the POS-ingest SLO, and a query
  # slower than the whole gateway budget is worth a log line whatever it is.
  parameter {
    name  = "log_min_duration_statement"
    value = "500"
  }

  parameter {
    name  = "log_connections"
    value = "1"
  }

  parameter {
    name  = "log_disconnections"
    value = "1"
  }

  # Statement timeout, 30 seconds. Nothing on the price path takes 30 seconds;
  # a query that does is a runaway, and a runaway holding a lock on a price
  # aggregate blocks every subsequent update to it.
  parameter {
    name  = "statement_timeout"
    value = "30000"
  }

  # An idle transaction holding a snapshot prevents vacuum and grows bloat
  # indefinitely. Sixty seconds is generous for a service that commits per
  # command.
  parameter {
    name  = "idle_in_transaction_session_timeout"
    value = "60000"
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_db_parameter_group" "this" {
  name_prefix = "${var.name_prefix}-aurora-"
  family      = "aurora-postgresql16"
  description = "USSLP Aurora PostgreSQL instance parameters"

  parameter {
    name  = "log_min_duration_statement"
    value = "500"
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

resource "aws_rds_cluster" "this" {
  cluster_identifier = "${var.name_prefix}-aurora"
  engine             = "aurora-postgresql"
  engine_version     = var.engine_version
  database_name      = "usslp"
  master_username    = var.master_username

  # Terraform generates the password into Secrets Manager and rotates it there.
  # The alternative — a password in a variable — puts it in state, in a plan
  # output, and in whatever CI log printed the plan.
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.kms_key_arn

  db_subnet_group_name            = aws_db_subnet_group.this.name
  vpc_security_group_ids          = [aws_security_group.aurora.id]
  db_cluster_parameter_group_name = aws_rds_cluster_parameter_group.this.name

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  # IAM database authentication. A service authenticates with the same IRSA
  # role it uses for S3 and KMS — one identity, revoked in one place — instead
  # of a long-lived password that has to be rotated and distributed.
  iam_database_authentication_enabled = true

  backup_retention_period      = var.backup_retention_days
  preferred_backup_window      = var.preferred_backup_window
  preferred_maintenance_window = var.preferred_maintenance_window
  copy_tags_to_snapshot        = true

  deletion_protection = var.deletion_protection

  # A final snapshot on delete. The identifier is timestamped because AWS
  # refuses to reuse one, and a destroy that fails on a name collision at 2 a.m.
  # is a destroy somebody will retry with skip_final_snapshot.
  skip_final_snapshot       = false
  final_snapshot_identifier = "${var.name_prefix}-aurora-final-${formatdate("YYYYMMDDhhmmss", timestamp())}"

  enabled_cloudwatch_logs_exports = ["postgresql"]

  # Backtrack is not available for Aurora PostgreSQL (it is a MySQL feature), so
  # point-in-time recovery through the retention window above is the mechanism.

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-aurora"
  })

  lifecycle {
    ignore_changes = [
      # Regenerated on every plan by formatdate/timestamp; without this the
      # cluster shows a permanent diff.
      final_snapshot_identifier,
      # A minor-version upgrade applied in the maintenance window would
      # otherwise be reverted on the next apply.
      engine_version,
    ]
  }
}

resource "aws_rds_cluster_instance" "this" {
  count = 1 + var.reader_count

  identifier         = "${var.name_prefix}-aurora-${count.index}"
  cluster_identifier = aws_rds_cluster.this.id
  instance_class     = var.instance_class
  engine             = aws_rds_cluster.this.engine
  engine_version     = aws_rds_cluster.this.engine_version

  db_parameter_group_name = aws_db_parameter_group.this.name

  # Instance 0 is the preferred writer; the readers are lower priority so that
  # a failover promotes a reader rather than shuffling the writer between
  # equally-ranked instances.
  promotion_tier = count.index

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = var.kms_key_arn
  performance_insights_retention_period = 465
  # 465 days rather than the free 7. When a query regression is found during a
  # quarterly review, the question is what it looked like before — and 7 days of
  # history cannot answer it.

  monitoring_interval = 30
  monitoring_role_arn = aws_iam_role.monitoring.arn

  # Minor versions are applied in the maintenance window, not on apply.
  auto_minor_version_upgrade = true

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-aurora-${count.index}"
    Role = count.index == 0 ? "writer" : "reader"
  })
}

data "aws_iam_policy_document" "monitoring_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "monitoring" {
  name_prefix        = "${var.name_prefix}-rds-mon-"
  assume_role_policy = data.aws_iam_policy_document.monitoring_assume.json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "monitoring" {
  role       = aws_iam_role.monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

output "cluster_endpoint" {
  description = "Writer endpoint."
  value       = aws_rds_cluster.this.endpoint
}

output "cluster_reader_endpoint" {
  description = "Reader endpoint, load-balanced across the replicas."
  value       = aws_rds_cluster.this.reader_endpoint
}

output "cluster_resource_id" {
  description = "Cluster resource id. IAM database authentication policies are written against this, not the cluster name."
  value       = aws_rds_cluster.this.cluster_resource_id
}

output "master_user_secret_arn" {
  description = "Secrets Manager ARN holding the generated master password."
  value       = aws_rds_cluster.this.master_user_secret[0].secret_arn
}

output "security_group_id" {
  description = "The cluster's security group."
  value       = aws_security_group.aurora.id
}

output "port" {
  description = "PostgreSQL port."
  value       = aws_rds_cluster.this.port
}
