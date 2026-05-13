# Terraform root — payment-platform.
#
# 部署:
#   terraform init -backend-config=backend-prod.hcl
#   terraform plan  -var-file=prod.tfvars
#   terraform apply -var-file=prod.tfvars
#
# State 落 S3 + DynamoDB lock (生产);本地开发可用 local backend.
#
# 资源拓扑:
#   modules/network        VPC / subnets / NAT / IGW / TGW
#   modules/eks            EKS cluster + node groups
#   modules/rds            RDS Aurora MySQL 3.0 (兼容 MySQL 8.0)
#   modules/elasticache    Redis cluster mode (Sentinel via cluster mode disabled)
#   modules/msk            MSK Kafka 3-broker
#   modules/s3             备份 / 数据湖 / Tempo storage buckets
#   modules/iam            IAM roles for k8s service accounts (IRSA)
#   modules/observability  Tempo / Loki / Grafana (在 EKS 上,通过 kubectl provider)

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws        = { source = "hashicorp/aws",        version = "~> 5.40" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.27" }
    helm       = { source = "hashicorp/helm",       version = "~> 2.13" }
    random     = { source = "hashicorp/random",     version = "~> 3.6" }
  }

  backend "s3" {
    bucket         = "payment-platform-tfstate"
    key            = "root/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "payment-platform-tfstate-lock"
    encrypt        = true
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project     = "payment-platform"
      Environment = var.env
      ManagedBy   = "terraform"
      Owner       = "platform-team"
    }
  }
}

# ── 网络层 ──────────────────────────────────────────────────────
module "network" {
  source = "./modules/network"

  env             = var.env
  region          = var.region
  vpc_cidr        = var.vpc_cidr
  azs             = var.azs
  public_subnets  = var.public_subnets
  private_subnets = var.private_subnets
  enable_nat_gateway = true
  single_nat_gateway = var.env == "dev"   # dev 节省成本只装 1 个 NAT;prod 每个 AZ 一个
}

# ── EKS ────────────────────────────────────────────────────────
module "eks" {
  source = "./modules/eks"

  cluster_name    = "payment-${var.env}"
  cluster_version = "1.28"
  vpc_id          = module.network.vpc_id
  subnet_ids      = module.network.private_subnet_ids

  node_groups = {
    general = {
      desired_size = var.eks_node_general_desired
      min_size     = 3
      max_size     = 30
      instance_types = ["m6i.2xlarge"]
      labels = { role = "general" }
    }
    data = {
      desired_size = var.eks_node_data_desired
      min_size     = 3
      max_size     = 10
      instance_types = ["r6i.2xlarge"]
      labels = { role = "data-node" }
      taints = [{ key = "data-node", value = "true", effect = "NO_SCHEDULE" }]
    }
    pci = {
      desired_size = 3
      min_size     = 3
      max_size     = 6
      instance_types = ["c6i.xlarge"]
      labels = { role = "pci", "compliance.payment/pci" = "saqd" }
      taints = [{ key = "pci", value = "saqd", effect = "NO_SCHEDULE" }]
    }
  }
}

# ── RDS Aurora MySQL ───────────────────────────────────────────
module "rds" {
  source = "./modules/rds"

  env                 = var.env
  vpc_id              = module.network.vpc_id
  subnet_ids          = module.network.private_subnet_ids
  allowed_cidr_blocks = [var.vpc_cidr]

  cluster_identifier   = "payment-${var.env}"
  database_name        = "payment"
  master_username      = "payment_admin"
  instance_count       = var.env == "prod" ? 3 : 2
  instance_class       = var.env == "prod" ? "db.r6i.4xlarge" : "db.r6i.large"
  backup_retention_days = var.env == "prod" ? 30 : 7
  enable_pitr          = true
  enable_iam_auth      = true
}

# ── ElastiCache Redis ──────────────────────────────────────────
module "elasticache" {
  source = "./modules/elasticache"

  env                 = var.env
  vpc_id              = module.network.vpc_id
  subnet_ids          = module.network.private_subnet_ids
  allowed_cidr_blocks = [var.vpc_cidr]

  cluster_id      = "payment-${var.env}"
  node_type       = var.env == "prod" ? "cache.r6g.xlarge" : "cache.r6g.large"
  num_shards      = var.env == "prod" ? 3 : 1
  replicas_per_shard = 2
  automatic_failover = true
  multi_az           = var.env == "prod"
  snapshot_retention_days = 7
}

# ── MSK Kafka ──────────────────────────────────────────────────
module "msk" {
  source = "./modules/msk"

  env             = var.env
  vpc_id          = module.network.vpc_id
  subnet_ids      = module.network.private_subnet_ids

  cluster_name    = "payment-${var.env}"
  kafka_version   = "3.6.0"
  num_brokers     = 3
  instance_type   = var.env == "prod" ? "kafka.m5.2xlarge" : "kafka.m5.large"
  ebs_volume_size = var.env == "prod" ? 1000 : 200
}

# ── S3 buckets ─────────────────────────────────────────────────
module "s3" {
  source = "./modules/s3"

  env = var.env
  buckets = {
    backups       = { name = "payment-${var.env}-backups",       retention_days = 90 }
    data_lake     = { name = "payment-${var.env}-data-lake",     retention_days = 1825 }
    tempo_traces  = { name = "payment-${var.env}-tempo",         retention_days = 30 }
    loki_logs     = { name = "payment-${var.env}-loki",          retention_days = 30 }
    audit_log     = { name = "payment-${var.env}-audit",         retention_days = 2555 }
  }
}

# ── IAM IRSA roles for K8s SAs ─────────────────────────────────
module "iam" {
  source = "./modules/iam"

  cluster_name           = module.eks.cluster_name
  cluster_oidc_provider  = module.eks.oidc_provider_arn

  irsa_bindings = {
    "payment/external-secrets"        = { policy_arns = [] }
    "payment/aws-load-balancer"       = { policy_arns = ["arn:aws:iam::aws:policy/ElasticLoadBalancingFullAccess"] }
    "payment/cluster-autoscaler"      = { policy_arns = [] }
    "payment/mysql-backup"            = { policy_arns = ["arn:aws:iam::aws:policy/AmazonS3FullAccess"] }
    "monitoring/tempo"                = { policy_arns = ["arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"] }
  }
}

# ── Observability (Tempo / Loki / Grafana via Helm) ────────────
module "observability" {
  source = "./modules/observability"

  cluster_endpoint = module.eks.cluster_endpoint
  cluster_ca       = module.eks.cluster_ca_data

  tempo_s3_bucket = module.s3.bucket_names["tempo_traces"]
  loki_s3_bucket  = module.s3.bucket_names["loki_logs"]
}
