# USSLP infrastructure

```
modules/
  network       VPC, three subnet tiers across three AZs, endpoints, flow logs
  eks           control plane, three node groups, OIDC provider, addons
  msk           Kafka in KRaft mode, sized from canon.AllStreams()
  aurora        Aurora PostgreSQL 16
  elasticache   Redis, cluster mode
  s3-firmware   firmware artifacts, Object Lock in COMPLIANCE mode
  kms           four keys, one per data domain, all single-region
  iam-irsa      one role per service, never one shared role
regions/
  us-east-1     primary
  eu-west-1     EU data residency
  ap-south-1    India data localisation
check-hcl.py    a structural checker for environments with no terraform binary
```

```bash
cd deploy/terraform/regions/us-east-1
terraform init
terraform plan
terraform output helm_values     # replaces the REPLACE-ME placeholders in the chart values
```

**This has never been applied against a real account, and it contains no real
account.** Every ARN, endpoint, hosted zone and bucket name is either derived
from a variable or is a literal `REPLACE-ME`. `make tf-check` runs
`terraform fmt -check` and `terraform validate` when terraform is available, and
a structural HCL checker when it is not — see `check-hcl.py`'s docstring for
exactly what each proves.

---

## Single-file modules

Each module is one `main.tf` containing its `terraform` block, variables,
resources and outputs, rather than the conventional three files. For modules
this size the three-file split means reading a variable's declaration in one
file, its use in another and its default in a third; keeping them adjacent makes
the module readable top to bottom. The trade is that `main.tf` is long. The
checker indexes variables and outputs per directory either way.

---

## Decisions worth knowing about

### The data tier has no route to the internet

Three subnet tiers: public (NAT gateways and load balancers only), private (EKS
nodes), and **data** (MSK, Aurora, ElastiCache). The data tier's route table has
no default route at all.

That absence is the control. A subnet with no path out cannot reach the internet
even if something in it is compromised and even if a security group is
misconfigured. For the two residency-enforced regions that is the difference
between "we intend not to send EU data elsewhere" and "there is no route by which
it could go".

### Every KMS key is single-region

`multi_region = false`, four keys — `events`, `database`, `firmware`,
`secrets` — each with a Deny on `aws:RequestedRegion != <this region>`.

A multi-region key has replicas whose material is identical everywhere, which
would let a caller in us-east-1 decrypt an eu-west-1 ciphertext. The cost of
refusing that is that a cross-region restore is impossible without re-encrypting.
That is the intended cost.

One key per domain rather than one per region, because a single key means
revoking access to the firmware bucket also revokes access to the event stream.

### The MSK cluster is sized by arithmetic, not by feel

The catalogue is 5,472 partitions (`canon.AllStreams()`); at replication factor
3 that is 16,416 partition replicas, and across six brokers 2,736 each — inside
MSK's 4,000-per-broker ceiling with headroom. A `precondition` fails the plan if
a change pushes it past 3,500, with the numbers in the error message.

Storage is sized the same way, and the assumptions it rests on are written into
the module rather than left implicit. `broker_volume_gb` defaults to 12,000 —
72 TB across six brokers — against a retention model whose driver is
`label-telemetry` at 167,000 events per second for 72 hours: 30.3 TB per replica,
90.9 TB at replication factor 3 uncompressed, 30.3 TB on disk at a conservative
zstd 3:1. The full arithmetic is above the variable and in
`docs/architecture/scalability.md` §2.4.

Two assumptions carry it, and the number is wrong without either:

- **Producers compress with zstd at 3:1 or better.** The broker is left on
  `compression.type=producer` because broker-side recompression costs CPU on the
  price path, so an uncompressed producer overruns the volume. That is a client
  misconfiguration, not a sizing error, and nothing here catches it yet.
- **`audit-log` keeps a 7-day *broker* window, not 365 days.** The catalogue's
  8,760 hours is the compliance retention of the record; on brokers it would be
  roughly 7.6 PB, which is not a Kafka retention. The design that satisfies it —
  a broker replay buffer, a Kafka Connect S3 sink into the Object Lock bucket
  under the region-local `events` key, and a read path over the archive — is
  documented in the module's `log.retention.hours` block. The first part is
  provisioned; the other two are not built.

`unclean.leader.election.enable=false` and `auto.create.topics.enable=false` are
both non-negotiable: a leader that is not in sync has lost writes, and a topic
auto-created with the wrong partition count silently destroys the per-key
ordering the platform is built on.

MSK creates no topics. That is the Helm pre-install Job's work, because it runs
inside the VPC where it can reach the brokers.

### The firmware bucket is Object Lock COMPLIANCE, not GOVERNANCE

GOVERNANCE mode can be bypassed by anyone holding
`s3:BypassGovernanceRetention`, which makes the protection exactly as strong as
the IAM policy around it. COMPLIANCE cannot be bypassed by anyone, including the
account root, for the retention period — default ten years, validated at minimum
seven, because a smart label has a 7–10 year battery life and the firmware it is
running must remain retrievable for as long as it might still be running it.

A mistakenly uploaded artifact occupies storage for a decade. That is the correct
trade for an artifact whose integrity the fleet depends on.

`prevent_destroy` is set, because S3 refuses to delete a bucket with locked
objects anyway and it is better to fail at plan time with a readable message.

### One IRSA role per service

The trust policy names the namespace **and** the service account. Naming only the
namespace would let any pod in it assume any role — the same thing as one shared
role with more steps.

The OTA service is the only role with firmware bucket access. Every role carries
an explicit Deny on `kafka-cluster:DeleteTopic`, including the topic-provisioning
Job's, because deleting a topic here destroys a compliance record or a compacted
read-model source.

### No peering, anywhere

The network module creates no VPC peering and no transit gateway attachment in
any region — including us-east-1, which has no residency obligation. A peering
connection is a route by which data could leave, and a route that exists in one
region's module is a route somebody will enable in another.

Cross-region traffic that genuinely must exist is control-plane, not customer
data, and goes over the public internet with mTLS where it is visible.

### Terraform state is region-local for the residency regions

The commented backend blocks in `eu-west-1` and `ap-south-1` point at in-region
buckets. State contains that region's resource identifiers and endpoint names,
and putting it in a us-east-1 bucket would be the residency leak nobody thinks to
look for.

---

## What is deliberately not here

**Kubernetes workloads.** The chart deploys those. Terraform stops at the
cluster.

**The ArgoCD installation, cert-manager, the External Secrets Operator,
Gatekeeper, Kyverno, prometheus-adapter.** Cluster bootstrap, with a different
lifecycle. Terraform creates the IRSA role the External Secrets Operator needs;
installing it is the bootstrap's job.

**Secret values.** Terraform generates Aurora's master password and
ElastiCache's auth token into Secrets Manager (`manage_master_user_password`) so
they are never in a variable, in state as plaintext, or in a plan output. Every
other secret is written out of band and read by the operator.

**Route 53 records and ACM certificates.** They need a real hosted zone. The
ingress hostnames in the chart's values files are `REPLACE-ME` for the same
reason.
