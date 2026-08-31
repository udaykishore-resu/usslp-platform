# USSLP — regional network.
#
# One VPC per region, three availability zones, three subnet tiers.
#
# ---------------------------------------------------------------------------
# Why three tiers and not two
# ---------------------------------------------------------------------------
#   public   — NAT gateways and the load balancers Store Gateway Units dial.
#              Nothing else. No workload ever lands here.
#   private  — EKS nodes. Egress through NAT, no inbound from the internet.
#   data     — MSK, Aurora, ElastiCache. No NAT route at all.
#
# The data tier is the one that earns its keep. A subnet with no route to a NAT
# gateway cannot reach the internet even if something in it is compromised and
# even if a security group is misconfigured, because there is no path. For the
# two regions with data-residency obligations that is not a nice-to-have: it is
# the difference between "we intend not to send EU data elsewhere" and "there is
# no route by which it could go".
#
# ---------------------------------------------------------------------------
# Cross-region
# ---------------------------------------------------------------------------
# There is deliberately no VPC peering and no transit gateway attachment in this
# module. eu-west-1 and ap-south-1 are residency-enforced, and a peering
# connection is a route by which data could leave. Cross-region traffic that
# genuinely must exist — control-plane calls, not customer data — goes over the
# public internet with mTLS, where it is visible and auditable.

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
  description = "Prefix for every resource name, e.g. usslp-prod-use1."
  type        = string
}

variable "region" {
  description = "AWS region. Must match global.region in the Helm values for this cluster, because that value becomes USSLP_REGION and therefore a const label on every metric."
  type        = string
}

variable "vpc_cidr" {
  description = "VPC CIDR. /16 gives room for three tiers across three AZs with /20 subnets and space left for a fourth AZ if the region grows one."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block."
  }
}

variable "availability_zones" {
  description = "AZs to spread across. Three is the design point: the label-service PDB minimum of 4 out of a 6-pod floor assumes two pods per zone, so that losing a zone leaves four."
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 2
    error_message = "At least two availability zones are required; three is the design point."
  }
}

variable "single_nat_gateway" {
  description = "Use one NAT gateway for every private subnet instead of one per AZ. Cheaper, and a single-AZ failure takes egress from every AZ. Acceptable in dev and staging; never in production."
  type        = bool
  default     = false
}

variable "enable_flow_logs" {
  description = "VPC flow logs to CloudWatch. On by default: in a residency-enforced region the flow log is the evidence that no traffic left."
  type        = bool
  default     = true
}

variable "flow_log_retention_days" {
  description = "Flow log retention."
  type        = number
  default     = 90
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

locals {
  az_count = length(var.availability_zones)

  # /20 subnets carved deterministically out of the VPC CIDR. Deterministic
  # rather than computed from a list, so that adding an AZ does not renumber —
  # and therefore recreate — the existing subnets.
  public_subnets  = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i)]
  private_subnets = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 4)]
  data_subnets    = [for i in range(local.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 8)]

  nat_count = var.single_nat_gateway ? 1 : local.az_count

  common_tags = merge(var.tags, {
    "usslp.io/region" = var.region
    ManagedBy         = "terraform"
  })
}

# ---------------------------------------------------------------------------
# VPC
# ---------------------------------------------------------------------------

resource "aws_vpc" "this" {
  cidr_block = var.vpc_cidr

  # Both required by EKS: the cluster's service discovery depends on private
  # hosted-zone resolution inside the VPC.
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-vpc"
  })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-igw"
  })
}

# ---------------------------------------------------------------------------
# Subnets
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count = local.az_count

  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_subnets[count.index]
  availability_zone       = var.availability_zones[count.index]
  map_public_ip_on_launch = false
  # false deliberately. The only things in the public tier are NAT gateways and
  # load balancers, both of which get an explicit EIP. Auto-assigning a public
  # IP would mean anything accidentally launched here is on the internet.

  tags = merge(local.common_tags, {
    Name                     = "${var.name_prefix}-public-${var.availability_zones[count.index]}"
    "kubernetes.io/role/elb" = "1"
    Tier                     = "public"
  })
}

resource "aws_subnet" "private" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_subnets[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name                              = "${var.name_prefix}-private-${var.availability_zones[count.index]}"
    "kubernetes.io/role/internal-elb" = "1"
    Tier                              = "private"
  })
}

resource "aws_subnet" "data" {
  count = local.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.data_subnets[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-data-${var.availability_zones[count.index]}"
    Tier = "data"
  })
}

# ---------------------------------------------------------------------------
# NAT
# ---------------------------------------------------------------------------

resource "aws_eip" "nat" {
  count = local.nat_count

  domain = "vpc"

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-nat-${count.index}"
  })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-nat-${count.index}"
  })

  depends_on = [aws_internet_gateway.this]
}

# ---------------------------------------------------------------------------
# Route tables
# ---------------------------------------------------------------------------

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-public"
  })
}

resource "aws_route_table_association" "public" {
  count = local.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  count = local.az_count

  vpc_id = aws_vpc.this.id

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-private-${var.availability_zones[count.index]}"
  })
}

resource "aws_route_table_association" "private" {
  count = local.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# The data tier's route table has NO default route. That absence is the control:
# MSK, Aurora and ElastiCache cannot reach the internet, whatever a security
# group says, because there is no path out of the subnet.
resource "aws_route_table" "data" {
  vpc_id = aws_vpc.this.id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-data"
    Note = "no default route by design"
  })
}

resource "aws_route_table_association" "data" {
  count = local.az_count

  subnet_id      = aws_subnet.data[count.index].id
  route_table_id = aws_route_table.data.id
}

# ---------------------------------------------------------------------------
# VPC endpoints
#
# S3 and DynamoDB as gateway endpoints — they are free, they attach to a route
# table, and they are what lets the data tier reach S3 with no NAT. The firmware
# bucket is read from EKS nodes, which do have NAT, but routing that traffic
# through a gateway endpoint keeps it off the public internet and off the NAT
# bill: a fleet-wide firmware rollout is a lot of gigabytes.
# ---------------------------------------------------------------------------

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"

  route_table_ids = concat(
    aws_route_table.private[*].id,
    [aws_route_table.data.id],
  )

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-s3"
  })
}

# Interface endpoints for the services IRSA needs. STS in particular: without
# it, every pod assuming a role via IRSA makes a call that leaves the VPC.
resource "aws_vpc_endpoint" "interface" {
  for_each = toset([
    "sts",
    "kms",
    "secretsmanager",
    "ecr.api",
    "ecr.dkr",
    "logs",
    "monitoring",
  ])

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.vpc_endpoints.id]
  private_dns_enabled = true

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-${replace(each.value, ".", "-")}"
  })
}

resource "aws_security_group" "vpc_endpoints" {
  name_prefix = "${var.name_prefix}-vpce-"
  description = "HTTPS from inside the VPC to the interface endpoints"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "HTTPS from the VPC"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-vpce"
  })

  lifecycle {
    create_before_destroy = true
  }
}

# ---------------------------------------------------------------------------
# Flow logs
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name              = "/aws/vpc/${var.name_prefix}/flow-logs"
  retention_in_days = var.flow_log_retention_days

  tags = local.common_tags
}

data "aws_iam_policy_document" "flow_logs_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name_prefix        = "${var.name_prefix}-flow-"
  assume_role_policy = data.aws_iam_policy_document.flow_logs_assume.json

  tags = local.common_tags
}

data "aws_iam_policy_document" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  statement {
    effect = "Allow"

    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogGroups",
      "logs:DescribeLogStreams",
    ]

    resources = ["${aws_cloudwatch_log_group.flow_logs[0].arn}:*"]
  }
}

resource "aws_iam_role_policy" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name_prefix = "${var.name_prefix}-flow-"
  role        = aws_iam_role.flow_logs[0].id
  policy      = data.aws_iam_policy_document.flow_logs[0].json
}

resource "aws_flow_log" "this" {
  count = var.enable_flow_logs ? 1 : 0

  vpc_id               = aws_vpc.this.id
  traffic_type         = "ALL"
  iam_role_arn         = aws_iam_role.flow_logs[0].arn
  log_destination      = aws_cloudwatch_log_group.flow_logs[0].arn
  log_destination_type = "cloud-watch-logs"
  # One-minute aggregation rather than ten. In a residency investigation the
  # question is "did anything leave, and when", and a ten-minute bucket is a
  # ten-minute uncertainty.
  max_aggregation_interval = 60

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-flow-logs"
  })
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "vpc_id" {
  description = "VPC id."
  value       = aws_vpc.this.id
}

output "vpc_cidr" {
  description = "VPC CIDR block."
  value       = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  description = "Public subnet ids, one per AZ."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnet ids, one per AZ. EKS nodes live here."
  value       = aws_subnet.private[*].id
}

output "data_subnet_ids" {
  description = "Data subnet ids, one per AZ. No route to the internet."
  value       = aws_subnet.data[*].id
}

output "availability_zones" {
  description = "The AZs this VPC spans."
  value       = var.availability_zones
}
