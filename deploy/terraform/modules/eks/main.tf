# USSLP — EKS.
#
# ===========================================================================
# Three node groups, and why
# ===========================================================================
# Not one. The price path and analytics have opposite requirements, and putting
# them on the same nodes means the noisy one decides how the quiet one performs.
#
#   platform   on-demand, taint usslp.io/dedicated=platform:NoSchedule.
#              Everything the Helm chart deploys. On-demand and not Spot: a
#              Spot interruption is a two-minute warning, and a store-wide
#              fan-out in flight when it arrives has 40,000 labels left to
#              publish. The PodDisruptionBudget does not apply to a Spot
#              reclaim — that is an involuntary disruption — so the only
#              protection is not being on Spot.
#
#   analytics  Spot, taint usslp.io/dedicated=analytics:NoSchedule.
#              Analytics is replayable from the stream; an interrupted job is
#              a delay. This is exactly the workload Spot is for, and the
#              usslp-analytics PriorityClass says the same thing at the
#              scheduler level.
#
#   system     on-demand, small. CoreDNS, the CNI, the metrics server,
#              Gatekeeper, Kyverno, the External Secrets Operator, ArgoCD.
#              Separated so that a workload node group scaling to zero, or
#              being cordoned wholesale during an incident, does not take the
#              admission controllers with it — which would make the cluster
#              unable to admit the replacement pods.

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
}

variable "name_prefix" {
  description = "Prefix for every resource name."
  type        = string
}

variable "region" {
  description = "AWS region."
  type        = string
}

variable "kubernetes_version" {
  description = "EKS control-plane version. The chart declares kubeVersion >= 1.28."
  type        = string
  default     = "1.30"
}

variable "vpc_id" {
  description = "VPC id."
  type        = string
}

variable "private_subnet_ids" {
  description = "Private subnet ids for the nodes and the control-plane ENIs."
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnet ids, for the load balancers Store Gateway Units dial."
  type        = list(string)
  default     = []
}

variable "endpoint_public_access" {
  description = "Expose the Kubernetes API publicly. Off in production: the API is reached through a VPN or a bastion, and an API server on the internet is one credential leak away from a cluster."
  type        = bool
  default     = false
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach a public API endpoint. Only meaningful when endpoint_public_access is true, and 0.0.0.0/0 there is refused by the validation below."
  type        = list(string)
  default     = []

  validation {
    condition     = !contains(var.endpoint_public_access_cidrs, "0.0.0.0/0")
    error_message = "0.0.0.0/0 is not an acceptable value for endpoint_public_access_cidrs on a cluster carrying retail pricing data."
  }
}

variable "node_groups" {
  description = "Node group definitions. The defaults implement the three-group split described in the header."

  type = map(object({
    instance_types = list(string)
    capacity_type  = string
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = number
    labels         = map(string)
    taints = list(object({
      key    = string
      value  = string
      effect = string
    }))
  }))

  default = {
    platform = {
      instance_types = ["m6i.2xlarge", "m6a.2xlarge", "m5.2xlarge"]
      capacity_type  = "ON_DEMAND"
      min_size       = 6
      max_size       = 60
      desired_size   = 9
      disk_size_gb   = 200
      labels = {
        "usslp.io/workload" = "platform"
      }
      taints = [{
        key    = "usslp.io/dedicated"
        value  = "platform"
        effect = "NO_SCHEDULE"
      }]
    }

    analytics = {
      instance_types = ["r6i.2xlarge", "r6a.2xlarge", "r5.2xlarge"]
      capacity_type  = "SPOT"
      min_size       = 0
      max_size       = 30
      desired_size   = 3
      disk_size_gb   = 500
      labels = {
        "usslp.io/workload" = "analytics"
      }
      taints = [{
        key    = "usslp.io/dedicated"
        value  = "analytics"
        effect = "NO_SCHEDULE"
      }]
    }

    system = {
      instance_types = ["m6i.large", "m6a.large"]
      capacity_type  = "ON_DEMAND"
      min_size       = 3
      max_size       = 6
      desired_size   = 3
      disk_size_gb   = 100
      labels = {
        "usslp.io/workload" = "system"
      }
      taints = []
    }
  }
}

variable "kms_key_arn" {
  description = "KMS key for envelope-encrypting Kubernetes Secrets at rest in etcd."
  type        = string
}

variable "log_retention_days" {
  description = "Control-plane log retention."
  type        = number
  default     = 90
}

variable "tags" {
  description = "Tags applied to every resource."
  type        = map(string)
  default     = {}
}

locals {
  cluster_name = "${var.name_prefix}-eks"

  common_tags = merge(var.tags, {
    ManagedBy         = "terraform"
    "usslp.io/region" = var.region
  })
}

# ---------------------------------------------------------------------------
# Control plane
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "cluster_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "cluster" {
  name_prefix        = "${var.name_prefix}-eks-"
  assume_role_policy = data.aws_iam_policy_document.cluster_assume.json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "cluster" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
    "arn:aws:iam::aws:policy/AmazonEKSVPCResourceController",
  ])

  role       = aws_iam_role.cluster.name
  policy_arn = each.value
}

resource "aws_security_group" "cluster" {
  name_prefix = "${var.name_prefix}-eks-cp-"
  description = "USSLP EKS control plane"
  vpc_id      = var.vpc_id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-eks-cp"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_cloudwatch_log_group" "cluster" {
  name              = "/aws/eks/${local.cluster_name}/cluster"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.kms_key_arn

  tags = local.common_tags
}

resource "aws_eks_cluster" "this" {
  name     = local.cluster_name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids              = var.private_subnet_ids
    security_group_ids      = [aws_security_group.cluster.id]
    endpoint_private_access = true
    endpoint_public_access  = var.endpoint_public_access
    public_access_cidrs     = var.endpoint_public_access ? var.endpoint_public_access_cidrs : []
  }

  # Envelope encryption for Secrets in etcd. Without it, a Kubernetes Secret is
  # base64 in etcd and nothing more. The chart never templates secret data — it
  # is all ExternalSecrets pointing at a secret store — but the Secrets the
  # operator writes still land in etcd, and this is what protects them there.
  encryption_config {
    provider {
      key_arn = var.kms_key_arn
    }

    resources = ["secrets"]
  }

  enabled_cluster_log_types = [
    "api",
    "audit",
    "authenticator",
    "controllerManager",
    "scheduler",
  ]
  # All five. The audit log in particular is what answers "who changed this" in
  # a region with residency obligations, and it cannot be enabled retroactively
  # for an event that has already happened.

  access_config {
    # API, not the aws-auth ConfigMap. Access entries are IAM objects that can
    # be reviewed and revoked with IAM tooling; the ConfigMap is a YAML blob
    # that anyone with edit access to kube-system can rewrite.
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }

  tags = merge(local.common_tags, {
    Name = local.cluster_name
  })

  depends_on = [
    aws_iam_role_policy_attachment.cluster,
    aws_cloudwatch_log_group.cluster,
  ]
}

# ---------------------------------------------------------------------------
# OIDC provider — the foundation of IRSA
#
# Without this, a pod's only path to AWS credentials is the node's instance
# profile, which means every pod on a node has every permission any pod on that
# node needs. IRSA is what makes "the OTA service reads the firmware bucket and
# the Label Service does not" enforceable.
# ---------------------------------------------------------------------------

data "tls_certificate" "oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}

resource "aws_iam_openid_connect_provider" "this" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]

  tags = merge(local.common_tags, {
    Name = "${local.cluster_name}-oidc"
  })
}

# ---------------------------------------------------------------------------
# Node groups
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "node_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "node" {
  name_prefix        = "${var.name_prefix}-eks-node-"
  assume_role_policy = data.aws_iam_policy_document.node_assume.json

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
    # AmazonEKS_CNI_Policy is deliberately NOT attached to the node role. It is
    # attached to the VPC CNI addon's own IRSA role instead (below), so that a
    # compromised pod on a node cannot manipulate ENIs through the node's
    # instance profile.
  ])

  role       = aws_iam_role.node.name
  policy_arn = each.value
}

resource "aws_security_group" "node" {
  name_prefix = "${var.name_prefix}-eks-node-"
  description = "USSLP EKS nodes"
  vpc_id      = var.vpc_id

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-eks-node"
    # Required by the CNI and the load-balancer controller to discover the
    # cluster's nodes.
    "kubernetes.io/cluster/${local.cluster_name}" = "owned"
  })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_egress_rule" "node_all" {
  security_group_id = aws_security_group.node.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
  description       = "Node egress; pod-level egress is governed by NetworkPolicy"
}

resource "aws_vpc_security_group_ingress_rule" "node_self" {
  security_group_id            = aws_security_group.node.id
  referenced_security_group_id = aws_security_group.node.id
  ip_protocol                  = "-1"
  description                  = "Node to node"
}

resource "aws_vpc_security_group_ingress_rule" "node_from_cluster" {
  security_group_id            = aws_security_group.node.id
  referenced_security_group_id = aws_security_group.cluster.id
  from_port                    = 1025
  to_port                      = 65535
  ip_protocol                  = "tcp"
  description                  = "Control plane to kubelet and pods"
}

resource "aws_vpc_security_group_ingress_rule" "cluster_from_node" {
  security_group_id            = aws_security_group.cluster.id
  referenced_security_group_id = aws_security_group.node.id
  from_port                    = 443
  to_port                      = 443
  ip_protocol                  = "tcp"
  description                  = "Nodes to the API server"
}

resource "aws_launch_template" "node" {
  for_each = var.node_groups

  name_prefix = "${var.name_prefix}-${each.key}-"
  description = "USSLP ${each.key} nodes"

  vpc_security_group_ids = [aws_security_group.node.id]

  block_device_mappings {
    device_name = "/dev/xvda"

    ebs {
      volume_size           = each.value.disk_size_gb
      volume_type           = "gp3"
      encrypted             = true
      kms_key_id            = var.kms_key_arn
      delete_on_termination = true
      throughput            = 250
      iops                  = 5000
    }
  }

  metadata_options {
    # IMDSv2 required, and hop limit 1. Hop limit 1 is the important half: it
    # stops a container reaching the instance metadata service through the
    # bridge, which is the classic path from "an SSRF in a pod" to "the node's
    # instance profile credentials".
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  monitoring {
    enabled = true
  }

  tag_specifications {
    resource_type = "instance"

    tags = merge(local.common_tags, {
      Name                = "${var.name_prefix}-${each.key}"
      "usslp.io/workload" = each.key
    })
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_eks_node_group" "this" {
  for_each = var.node_groups

  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${var.name_prefix}-${each.key}"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.private_subnet_ids
  capacity_type   = each.value.capacity_type
  instance_types  = each.value.instance_types

  scaling_config {
    min_size     = each.value.min_size
    max_size     = each.value.max_size
    desired_size = each.value.desired_size
  }

  update_config {
    # One node at a time. The chart sets maxUnavailable 0 on every Deployment
    # and the PodDisruptionBudgets refuse to go below the floor, so a faster
    # node rollout would simply block on the budgets — slowly, and with a
    # confusing error. One at a time is the honest speed.
    max_unavailable = 1
  }

  launch_template {
    id      = aws_launch_template.node[each.key].id
    version = aws_launch_template.node[each.key].latest_version
  }

  labels = each.value.labels

  dynamic "taint" {
    for_each = each.value.taints

    content {
      key    = taint.value.key
      value  = taint.value.value
      effect = taint.value.effect
    }
  }

  tags = merge(local.common_tags, {
    Name = "${var.name_prefix}-${each.key}"
  })

  lifecycle {
    # The cluster autoscaler owns desired_size. Terraform reverting it on every
    # apply would scale the cluster back down mid-peak.
    ignore_changes = [scaling_config[0].desired_size]
  }

  depends_on = [aws_iam_role_policy_attachment.node]
}

# ---------------------------------------------------------------------------
# Addons
# ---------------------------------------------------------------------------

resource "aws_eks_addon" "this" {
  for_each = toset(["vpc-cni", "coredns", "kube-proxy", "aws-ebs-csi-driver"])

  cluster_name = aws_eks_cluster.this.name
  addon_name   = each.value

  # PRESERVE, not OVERWRITE. An addon whose configuration has been changed
  # deliberately — CoreDNS' replica count during an incident, say — should not
  # be silently reverted by the next Terraform apply.
  resolve_conflicts_on_create = "OVERWRITE"
  resolve_conflicts_on_update = "PRESERVE"

  tags = local.common_tags

  depends_on = [aws_eks_node_group.this]
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "cluster_name" {
  description = "EKS cluster name."
  value       = aws_eks_cluster.this.name
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 CA certificate for the API server."
  value       = aws_eks_cluster.this.certificate_authority[0].data
}

output "oidc_provider_arn" {
  description = "IAM OIDC provider ARN. The iam-irsa module's trust policies are written against this."
  value       = aws_iam_openid_connect_provider.this.arn
}

output "oidc_provider_url" {
  description = "OIDC issuer URL without the scheme, as an IAM condition key expects it."
  value       = replace(aws_eks_cluster.this.identity[0].oidc[0].issuer, "https://", "")
}

output "node_security_group_id" {
  description = "The nodes' security group. MSK, Aurora and ElastiCache take this as their allowed source."
  value       = aws_security_group.node.id
}

output "cluster_security_group_id" {
  description = "The control plane's security group."
  value       = aws_security_group.cluster.id
}

output "node_role_arn" {
  description = "The nodes' IAM role."
  value       = aws_iam_role.node.arn
}
