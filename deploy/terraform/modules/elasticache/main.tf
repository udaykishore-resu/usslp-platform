# USSLP — ElastiCache for Redis.
#
# As with Aurora: nothing in the Go tree connects to Redis today. The idempotency
# guard (platform/pkg/idem) and the tenant rate limiter
# (platform/internal/label/app/ratelimit.go) are in-process, which is correct for
# a single instance and insufficient across a fleet of them — a 24-hour
# idempotency window that only one pod knows about deduplicates nothing when the
# POS retries against a different pod.
#
# This module provisions the cluster that shared state is the documented port to.
#
# ===========================================================================
# What would live here, and what would not
# ===========================================================================
# WOULD:  the cross-pod idempotency guard (contract section 6 fixes the window at
#         24 hours); per-tenant rate-limit buckets; the label directory read
#         cache that the fan-out consults before resolving placement.
#
# WOULD NOT: anything durable. The durable record of a price change is the event
#         stream, and a cache that is treated as a source of truth is a cache
#         whose eviction is a data-loss incident. Persistence is therefore off
#         (see snapshot_retention_limit below) and the eviction policy is LRU,
#         both of which are statements that losing the contents is fine.

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
  description = "Data-tier subnet ids, one per AZ."
  type        = list(string)
}

variable "allowed_security_group_ids" {
  description = "Security groups permitted to reach 6379."
  type        = list(string)
  default     = []
}

variable "engine_version" {
  description = "Redis engine version."
  type        = string
  default     = "7.1"
}

variable "node_type" {
  description = "Node type."
  type        = string
  default     = "cache.r7g.large"
}

variable "shard_count" {
  description = "Number of shards. Cluster mode is on, so the keyspace is partitioned across shards and a single hot tenant does not saturate one node."
  type        = number
  default     = 3
}

variable "replicas_per_shard" {
  description = "Replicas per shard. One, in another AZ, so a node failure is a failover rather than an empty cache."
  type        = number
  default     = 1
}

variable "kms_key_arn" {
  description = "KMS key for encryption at rest. From the kms module's `database` domain."
  type        = string
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

resource "aws_elasticache_subnet_group" "this" {
  name_prefix = "${var.name_prefix}-redis-"
  subnet_ids  = var.subnet_ids
  description = "USSLP Redis — data tier"

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "redis" {
  name_prefix = "${var.name_prefix}-redis-"
  description = "USSLP ElastiCache Redis"
  vpc_id      = var.vpc_id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-redis"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "redis" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.redis.id
  referenced_security_group_id = each.value
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  description                  = "Redis from the cluster"
}

resource "aws_elasticache_parameter_group" "this" {
  name_prefix = "${var.name_prefix}-redis-"
  family      = "redis7"
  description = "USSLP Redis: a cache, not a database"

  # allkeys-lru, not noeviction. noeviction makes a full cache return errors to
  # writers, which for the idempotency guard would mean refusing to record that
  # a delivery had been seen — and a POS retry that is not recognised as a
  # duplicate becomes a duplicated price change.
  #
  # Evicting the oldest entry instead means the guard's window silently shortens
  # under memory pressure, which degrades deduplication rather than breaking it.
  parameter {
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }

  # Keyspace notifications for expiry, so a rate limiter can observe bucket
  # expiry rather than polling.
  parameter {
    name  = "notify-keyspace-events"
    value = "Ex"
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name_prefix}-redis"
  description          = "USSLP shared cache: idempotency guard, rate-limit buckets, directory cache"

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type
  port           = 6379

  # Cluster mode. Sharding matters here for a specific reason: the idempotency
  # guard is keyed per delivery and the rate limiter per tenant, so the keyspace
  # is naturally wide, and a single hot tenant's rate-limit bucket should not be
  # on the same node as every other tenant's.
  num_node_groups         = var.shard_count
  replicas_per_node_group = var.replicas_per_shard

  subnet_group_name    = aws_elasticache_subnet_group.this.name
  security_group_ids   = [aws_security_group.redis.id]
  parameter_group_name = aws_elasticache_parameter_group.this.name

  # Failover across AZs. Without automatic_failover_enabled a primary's death is
  # a manual intervention, and the idempotency guard being unavailable for
  # minutes means every POS retry in that window is treated as new.
  automatic_failover_enabled = true
  multi_az_enabled           = true

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn
  transit_encryption_enabled = true
  # The auth token is generated into Secrets Manager rather than passed in as a
  # variable, for the same reason as Aurora's master password.
  auth_token_update_strategy = "ROTATE"

  # No snapshots. This is a cache: the durable record of a price change is the
  # event stream, and a restored snapshot of a rate-limit bucket is worse than
  # an empty one — it would restore counters that no longer reflect anything.
  snapshot_retention_limit = 0

  maintenance_window = "sun:09:00-sun:10:00"

  # Notifications on failover, so that a primary moving is visible rather than
  # inferred from a latency graph.
  auto_minor_version_upgrade = true
  apply_immediately          = false

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.slow.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-redis"
  })

  lifecycle {
    ignore_changes = [
      engine_version,
      auth_token,
    ]
  }
}

resource "aws_cloudwatch_log_group" "slow" {
  name              = "/aws/elasticache/${var.name_prefix}/slow-log"
  retention_in_days = 14
  kms_key_id        = var.kms_key_arn

  tags = local.common_tags
}

output "primary_endpoint" {
  description = "Configuration endpoint for cluster mode. A cluster-mode-aware client discovers the shards from it."
  value       = aws_elasticache_replication_group.this.configuration_endpoint_address
}

output "port" {
  description = "Redis port."
  value       = aws_elasticache_replication_group.this.port
}

output "security_group_id" {
  description = "The cluster's security group."
  value       = aws_security_group.redis.id
}

output "replication_group_id" {
  description = "Replication group id."
  value       = aws_elasticache_replication_group.this.id
}
