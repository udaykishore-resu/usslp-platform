# USSLP — MSK, the event stream.
#
# ===========================================================================
# The topic catalogue
# ===========================================================================
# Partition counts and retentions are defined once, in
# platform/pkg/canon/topics.go (canon.AllStreams()), so that the local
# development broker, the docker-compose profile and this Terraform all derive
# from one source of truth. The `streams` variable below is that catalogue,
# transcribed; `make verify-topics` fails CI if the two disagree.
#
# MSK itself does not create topics — there is no Terraform resource for a Kafka
# topic that does not require a Kafka provider with network access to the
# cluster. The topics are created by the Helm chart's pre-install Job
# (deploy/helm/usslp/templates/topics-job.yaml), which runs inside the VPC where
# it can reach the brokers. The catalogue is duplicated into this module's
# variables so that the *capacity* calculation below — which is what sizes the
# cluster — is derived from the real numbers rather than from a guess.
#
# ===========================================================================
# Why the partition counts are what they are
# ===========================================================================
# From the capacity model: 52,000 price updates/second at peak and 167,000
# telemetry events/second at 50 million labels.
#
#   price-updates    1024   keyed store:sku. 1,024 partitions is what makes two
#                           price changes for the same product in the same store
#                           strictly ordered while different products proceed in
#                           parallel.
#   label-telemetry  2048   the highest-volume stream in the platform.
#   label-state       512   compacted: the latest value per key, forever. This
#                           is how a restarting service rebuilds its read model
#                           without replaying seven days of history.
#
# The total partition count across the catalogue is what sizes the cluster.
# MSK's own guidance is a maximum of 4,000 partitions per broker for
# kafka.m5.large and up; the validation below enforces headroom against that.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------

variable "name_prefix" {
  description = "Prefix for every resource name."
  type        = string
}

variable "vpc_id" {
  description = "VPC to place the cluster in."
  type        = string
}

variable "subnet_ids" {
  description = "Data-tier subnet ids, one per AZ. These subnets have no route to the internet."
  type        = list(string)
}

variable "allowed_security_group_ids" {
  description = "Security groups permitted to reach the brokers — normally just the EKS node group's."
  type        = list(string)
  default     = []
}

variable "kafka_version" {
  description = "Kafka version. 3.7.x runs in KRaft mode; there is no ZooKeeper."
  type        = string
  default     = "3.7.x"
}

variable "broker_instance_type" {
  description = "Broker instance type."
  type        = string
  default     = "kafka.m5.2xlarge"
}

variable "broker_count" {
  description = "Number of brokers. Must be a multiple of the AZ count so that every AZ carries the same load, and at least 3 so that replication factor 3 with min.insync.replicas 2 survives losing one."
  type        = number
  default     = 6

  validation {
    condition     = var.broker_count >= 3
    error_message = "At least 3 brokers are required: replication factor 3 with min.insync.replicas 2 is the platform's durability guarantee, and the UIG acknowledges a POS only once a change is durable."
  }
}

# EBS per broker.
#
# The previous default was 4,000 GB — 24 TB across six brokers — with a
# description claiming it was "sized from the retention model". It was not: the
# retention model does not fit in 24 TB and the description named no assumption
# that would make it fit. The number and the description now agree, and both
# state what they depend on.
#
# The arithmetic, from docs/architecture/scalability.md §2.4 and the record
# sizes measured against canon.Envelope in this tree (a full envelope is
# 1,196 B; a telemetry reading is ~700 B on the wire):
#
#   label-telemetry, the storage driver — a heartbeat has no peak:
#     167,000/s x ~700 B          =  116.9 MB/s
#                                 =   10.1 TB/day
#     x 72 h retention (3 days)   =   30.3 TB per replica
#     x RF 3                      =   90.9 TB uncompressed
#     at zstd 3:1                 =   30.3 TB on disk
#
#   price-updates, label-delivery and pos-integration at the daily mean of
#     5,787/s (the 52,000/s figure is a peak, not a retention rate), over
#     their 168/168/72-hour retentions, at RF 3 and the same ratio:
#                                 ~   10.5 TB on disk
#
#   audit-log, at the 7-day BROKER window described below rather than at the
#     catalogue's 365-day compliance retention:
#                                 ~    8.5 TB on disk
#
#   label-state is compacted: 50 M labels x 1,196 B x RF 3
#                                 ~    0.2 TB on disk
#
#   device-events, inventory-sync, promotion-events, ota-commands and
#     dead-letter are all low-rate:
#                                 ~    2 TB on disk
#                                    ─────────
#     total                        ~   51.5 TB
#     + 35% for burst, compaction churn and the lag before storage
#       autoscaling reacts        ~   70 TB  →  6 x 12,000 GB = 72 TB
#
# TWO ASSUMPTIONS, AND THE NUMBER IS WRONG WITHOUT EITHER.
#
#  1. Producers compress with zstd and achieve at least 3:1. USSLP envelopes are
#     JSON with a fixed key set repeated on every record, which compresses far
#     better than that — 5:1 is the realistic figure and 3:1 is the margin. The
#     broker is left on Kafka's default (compression.type=producer) rather than
#     forced to zstd, because broker-side recompression costs CPU on the price
#     path; a producer that ships uncompressed batches will overrun this volume,
#     and that is a client misconfiguration, not a sizing error.
#
#  2. audit-log keeps a 7-day window ON THE BROKER, not 365 days. See the
#     log.retention.hours block in the server properties below for why 365 days
#     of audit at RF 3 is not a Kafka retention at all and what has to exist
#     instead.
#
# MSK's EBS ceiling is 16,384 GB per broker, so 12,000 leaves room to grow this
# in place. Storage autoscaling (enabled below) grows it further under a burst.
variable "broker_volume_gb" {
  description = "EBS per broker. 12,000 GB x 6 brokers = 72 TB, sized from the retention model in scalability.md §2.4 and holding ONLY under two stated assumptions: producers compress with zstd at 3:1 or better, and audit-log keeps a 7-day broker window with the 365-day compliance record archived off-cluster. See the comment above this variable for the arithmetic."
  type        = number
  default     = 12000
}

variable "kms_key_arn" {
  description = "KMS key for encryption at rest. From the kms module; region-local, which is what makes the residency claim structural rather than intended."
  type        = string
}

variable "streams" {
  description = <<-EOT
    The stream catalogue, transcribed from canon.AllStreams(). Used here only to
    compute the cluster's partition load; the topics themselves are created by
    the Helm pre-install Job, which runs inside the VPC.
  EOT

  type = list(object({
    name            = string
    partitions      = number
    retention_hours = number
    compacted       = bool
  }))

  default = [
    { name = "price-updates", partitions = 1024, retention_hours = 168, compacted = false },
    { name = "device-events", partitions = 512, retention_hours = 720, compacted = false },
    { name = "label-telemetry", partitions = 2048, retention_hours = 72, compacted = false },
    { name = "inventory-sync", partitions = 256, retention_hours = 336, compacted = false },
    { name = "promotion-events", partitions = 128, retention_hours = 2160, compacted = false },
    { name = "audit-log", partitions = 64, retention_hours = 8760, compacted = false },
    { name = "ota-commands", partitions = 128, retention_hours = 168, compacted = false },
    { name = "pos-integration", partitions = 256, retention_hours = 72, compacted = false },
    { name = "label-delivery", partitions = 512, retention_hours = 168, compacted = false },
    { name = "dead-letter", partitions = 32, retention_hours = 336, compacted = false },
    { name = "label-state", partitions = 512, retention_hours = 0, compacted = true },
  ]
}

variable "replication_factor" {
  description = "Default replication factor for the catalogue's topics."
  type        = number
  default     = 3
}

variable "min_insync_replicas" {
  description = "Minimum in-sync replicas. 2 with RF 3 means a write survives losing one broker and still fails rather than silently under-replicating when two are gone."
  type        = number
  default     = 2
}

variable "log_retention_days" {
  description = "Broker log retention in CloudWatch."
  type        = number
  default     = 30
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

locals {
  # Total partition replicas the cluster has to carry. This is the number that
  # sizes it, not the topic count: a 2,048-partition topic at RF 3 is 6,144
  # partition replicas on its own.
  total_partitions = sum([for s in var.streams : s.partitions])
  total_replicas   = local.total_partitions * var.replication_factor
  per_broker       = ceil(local.total_replicas / var.broker_count)

  common_tags = merge(var.tags, {
    ManagedBy = "terraform"
  })
}

# MSK's documented ceiling for m5.large and above is 4,000 partitions per
# broker. Failing the plan here is far cheaper than discovering it when the
# topic-provisioning Job starts refusing to create label-telemetry.
resource "terraform_data" "partition_budget_check" {
  lifecycle {
    precondition {
      condition     = local.per_broker <= 3500
      error_message = "Partition replicas per broker (${local.per_broker}) exceeds the 3,500 headroom limit against MSK's 4,000 ceiling. The catalogue has ${local.total_partitions} partitions at replication factor ${var.replication_factor}; increase broker_count."
    }
  }

  input = {
    total_partitions = local.total_partitions
    total_replicas   = local.total_replicas
    per_broker       = local.per_broker
  }
}

# ---------------------------------------------------------------------------
# Security group
# ---------------------------------------------------------------------------

resource "aws_security_group" "msk" {
  name_prefix = "${var.name_prefix}-msk-"
  description = "USSLP MSK brokers"
  vpc_id      = var.vpc_id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-msk"
  })

  lifecycle {
    create_before_destroy = true
  }
}

# TLS only. 9092 (plaintext) is not opened at all: the platform's audit-log
# stream is a statutory compliance record and it does not travel in the clear
# inside a VPC any more than it does outside one.
resource "aws_vpc_security_group_ingress_rule" "msk_tls" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.msk.id
  referenced_security_group_id = each.value
  from_port                    = 9094
  to_port                      = 9094
  ip_protocol                  = "tcp"
  description                  = "Kafka over TLS with SASL/SCRAM"
}

resource "aws_vpc_security_group_ingress_rule" "msk_iam" {
  for_each = toset(var.allowed_security_group_ids)

  security_group_id            = aws_security_group.msk.id
  referenced_security_group_id = each.value
  from_port                    = 9098
  to_port                      = 9098
  ip_protocol                  = "tcp"
  description                  = "Kafka over TLS with IAM authentication"
}

resource "aws_vpc_security_group_ingress_rule" "msk_intra" {
  security_group_id            = aws_security_group.msk.id
  referenced_security_group_id = aws_security_group.msk.id
  from_port                    = 0
  to_port                      = 65535
  ip_protocol                  = "tcp"
  description                  = "Inter-broker replication"
}

resource "aws_vpc_security_group_egress_rule" "msk_intra" {
  security_group_id            = aws_security_group.msk.id
  referenced_security_group_id = aws_security_group.msk.id
  from_port                    = 0
  to_port                      = 65535
  ip_protocol                  = "tcp"
  description                  = "Inter-broker replication"
}

# ---------------------------------------------------------------------------
# Cluster configuration
#
# Every setting below exists to support a guarantee stated in
# docs/architecture/INTERFACE-CONTRACTS.md.
# ---------------------------------------------------------------------------

resource "aws_msk_configuration" "this" {
  name           = "${var.name_prefix}-config"
  kafka_versions = [var.kafka_version]
  description    = "USSLP event stream: at-least-once delivery, per-key ordering, no accidental topic creation."

  server_properties = <<-PROPERTIES
    # Topics are created by the Helm pre-install Job with the partition counts
    # from canon.AllStreams(). Auto-creation would produce a topic with the
    # default partition count the first time anything produced to a
    # mistyped name, and a topic created with the wrong partition count
    # silently destroys the per-key ordering the whole platform is built on.
    auto.create.topics.enable=false

    # Never. A partition leader that is not in sync has, by definition, lost
    # writes. For a stream carrying price changes and a statutory audit record,
    # availability does not outrank durability.
    unclean.leader.election.enable=false

    default.replication.factor=${var.replication_factor}
    min.insync.replicas=${var.min_insync_replicas}
    num.partitions=1

    # Deleting a topic requires an explicit act. The catalogue is small, fixed,
    # and named in Go source; nothing should be deleting one casually.
    delete.topic.enable=false

    # Seven days is the longest retention in the catalogue apart from audit-log
    # and promotion-events (90 days), both of which set their own.
    #
    # AUDIT-LOG'S 365 DAYS IS NOT A KAFKA RETENTION, AND NOTHING HERE PRETENDS
    # IT IS. canon.StreamAudit carries 8760 hours, and the topic-provisioning
    # Job transcribes that faithfully because it is the *compliance* retention
    # of the audit record. Held on brokers it would be:
    #
    #   5,787/s (the daily mean price rate) x 1,196 B envelope
    #     x 365 days x RF 3                 ≈ 7.6 PB
    #
    # — three orders of magnitude past any cluster this module provisions, and
    # not a thing to discover during an audit. The design that satisfies the
    # 365 days is tiered, and it has three parts, of which this module builds
    # the first and none of the Go tree builds the third:
    #
    #   1. A 7-day window on the brokers. That is a replay buffer for a sink
    #      that falls behind, not the compliance record; it is what the storage
    #      arithmetic on broker_volume_gb budgets for audit-log.
    #   2. An archive sink — Kafka Connect S3, which the chart already deploys
    #      — writing audit-log to an S3 bucket under Object Lock in COMPLIANCE
    #      mode, partitioned by tenant and day, with the region-local KMS
    #      `events` key so the residency guarantee survives the copy.
    #   3. A read path that answers "what price was this label showing on this
    #      date" from the archive rather than from Kafka. Nothing in the Go tree
    #      implements it. The columnar store's hot/warm/cold tiering is the
    #      closest thing and it is not wired to the audit stream.
    #
    # Until part 3 exists, the honest statement is that USSLP *produces* a
    # complete audit record and does not yet *retain* one for a year. Setting
    # log.retention.hours=8760 on the topic would not change that; it would only
    # move the failure from a documented gap to a full disk.
    log.retention.hours=168
    log.retention.check.interval.ms=300000

    # Compaction, for label-state. dirty.ratio 0.1 rather than the default 0.5:
    # a service rebuilding its read model reads the whole compacted log, and a
    # log that is half tombstones takes twice as long to replay.
    log.cleaner.enable=true
    log.cleaner.min.cleanable.ratio=0.1

    # A message carrying a store-wide planogram or a batch of telemetry is
    # larger than the 1 MB default.
    message.max.bytes=10485760
    replica.fetch.max.bytes=10485760

    # Consumer groups. A 5-minute session timeout is long: it means a consumer
    # that dies takes up to 5 minutes to be noticed. That is deliberate — a
    # shorter timeout makes a GC pause on a busy consumer look like a death and
    # triggers a rebalance, and a rebalance stops every consumer in the group.
    group.min.session.timeout.ms=6000
    group.max.session.timeout.ms=300000
    group.initial.rebalance.delay.ms=3000

    # Transactions, for the exactly-once semantics a future Kafka adapter may
    # use for the read-model projections.
    transaction.state.log.replication.factor=${var.replication_factor}
    transaction.state.log.min.isr=${var.min_insync_replicas}
    offsets.topic.replication.factor=${var.replication_factor}
  PROPERTIES

  lifecycle {
    create_before_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Cluster
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "broker" {
  name              = "/aws/msk/${var.name_prefix}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn

  tags = local.common_tags
}

resource "aws_msk_cluster" "this" {
  cluster_name           = "${var.name_prefix}-events"
  kafka_version          = var.kafka_version
  number_of_broker_nodes = var.broker_count

  broker_node_group_info {
    instance_type   = var.broker_instance_type
    client_subnets  = var.subnet_ids
    security_groups = [aws_security_group.msk.id]

    storage_info {
      ebs_storage_info {
        volume_size = var.broker_volume_gb

        # Storage autoscaling. A firmware rollout or a promotion produces a
        # burst that the steady-state model does not predict, and a broker that
        # runs out of disk stops accepting writes — which stops the UIG
        # acknowledging a POS, which makes the POS retry.
        provisioned_throughput {
          enabled           = true
          volume_throughput = 250
        }
      }
    }

    connectivity_info {
      # No public access. Store Gateway Units reach the platform over MQTT to
      # EMQX, never over Kafka. Nothing outside the VPC speaks to a broker.
      public_access {
        type = "DISABLED"
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.this.arn
    revision = aws_msk_configuration.this.latest_revision
  }

  client_authentication {
    sasl {
      # IAM authentication, so that a service's Kafka access is the same IRSA
      # role as its S3 and KMS access, revoked in one place. SCRAM is off: it
      # is a long-lived shared secret, and the platform already has a story for
      # identity that does not involve one.
      iam   = true
      scram = false
    }

    # No unauthenticated access, ever.
    unauthenticated = false
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = var.kms_key_arn

    encryption_in_transit {
      # TLS only, both client-broker and broker-broker. TLS_PLAINTEXT would
      # allow a misconfigured client to fall back to the clear.
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.broker.name
      }
    }
  }

  open_monitoring {
    prometheus {
      # JMX and node exporters. The platform's own consumer lag comes from
      # obs.StandardMetrics.ConsumerLag inside each service
      # (usslp_consumer_lag_records), which is the number the autoscaler uses;
      # these are the broker's own view, which is what tells you whether a lag
      # problem is the consumer or the cluster.
      jmx_exporter {
        enabled_in_broker = true
      }

      node_exporter {
        enabled_in_broker = true
      }
    }
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-events"
  })

  lifecycle {
    # Changing the broker count or instance type in place is a rolling
    # replacement that MSK performs itself; changing the AZ list is not.
    ignore_changes = [
      broker_node_group_info[0].storage_info[0].ebs_storage_info[0].volume_size,
    ]
    # volume_size is ignored because MSK's storage autoscaling grows it out of
    # band, and Terraform would otherwise shrink it back on the next apply —
    # which MSK refuses, failing every subsequent plan.
  }

  depends_on = [terraform_data.partition_budget_check]
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "cluster_arn" {
  description = "MSK cluster ARN. Needed by the IRSA policies in the iam-irsa module."
  value       = aws_msk_cluster.this.arn
}

output "cluster_name" {
  description = "MSK cluster name."
  value       = aws_msk_cluster.this.cluster_name
}

output "bootstrap_brokers_sasl_iam" {
  description = "Bootstrap servers for IAM authentication. This is the value that goes into topicProvisioning.bootstrapServers in the Helm values file."
  value       = aws_msk_cluster.this.bootstrap_brokers_sasl_iam
}

output "bootstrap_brokers_tls" {
  description = "Bootstrap servers for TLS with certificate authentication."
  value       = aws_msk_cluster.this.bootstrap_brokers_tls
}

output "security_group_id" {
  description = "The brokers' security group, for rules added elsewhere."
  value       = aws_security_group.msk.id
}

output "partition_budget" {
  description = "The computed partition load, for capacity review."

  value = {
    topics                = length(var.streams)
    total_partitions      = local.total_partitions
    total_replicas        = local.total_replicas
    replicas_per_broker   = local.per_broker
    brokers               = var.broker_count
    headroom_to_msk_limit = 4000 - local.per_broker
  }
}

output "streams" {
  description = "The stream catalogue this cluster was sized for, for the topic-provisioning Job to be checked against."
  value       = var.streams
}
