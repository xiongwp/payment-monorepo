# Staging IaC — AWS EKS + RDS + Route53 + ACM + ALB + Secrets Manager.
#
# 用法:
#   cd infra/terraform
#   terraform init
#   terraform plan -var-file=staging.tfvars
#   terraform apply -var-file=staging.tfvars

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws        = { source = "hashicorp/aws",        version = "~> 5.0" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.25" }
    helm       = { source = "hashicorp/helm",       version = "~> 2.12" }
  }
  backend "s3" {
    bucket         = "payment-platform-tfstate"
    key            = "staging/terraform.tfstate"
    region         = "us-east-1"
    dynamodb_table = "payment-platform-tflock"
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
    }
  }
}

# ── VPC ──
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.5"

  name = "pp-${var.env}"
  cidr = "10.0.0.0/16"

  azs             = ["${var.region}a", "${var.region}b", "${var.region}c"]
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24", "10.0.103.0/24"]

  enable_nat_gateway   = true
  single_nat_gateway   = var.env != "prod"  # staging 省一个 NAT (跟 prod 区分)
  enable_dns_hostnames = true
  enable_dns_support   = true
}

# ── EKS Cluster ──
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 20.0"

  cluster_name    = "pp-${var.env}"
  cluster_version = "1.29"

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  cluster_endpoint_public_access = true

  eks_managed_node_groups = {
    main = {
      desired_size = 3
      min_size     = 3
      max_size     = 10
      instance_types = ["m6i.large"]
      capacity_type  = "ON_DEMAND"
    }
    # spot node group 给非关键 workload (CI / bg job)
    spot = {
      desired_size = 2
      min_size     = 0
      max_size     = 20
      instance_types = ["m6i.large", "m6a.large", "m5.large"]
      capacity_type  = "SPOT"
      taints = [{
        key    = "spot"
        value  = "true"
        effect = "NO_SCHEDULE"
      }]
    }
  }

  enable_irsa = true   # IAM roles for service accounts
}

# ── RDS MySQL (主 + 跨 region replica 给 DR) ──
module "mysql" {
  source  = "terraform-aws-modules/rds/aws"
  version = "~> 6.5"

  identifier = "pp-${var.env}-mysql"
  engine     = "mysql"
  engine_version = "8.0.35"
  family         = "mysql8.0"
  major_engine_version = "8.0"
  instance_class = var.env == "prod" ? "db.r6i.2xlarge" : "db.t3.medium"

  allocated_storage      = var.env == "prod" ? 500 : 100
  max_allocated_storage  = 2000
  storage_encrypted      = true
  kms_key_id             = aws_kms_key.rds.arn

  db_name  = "payment"
  username = "platform_admin"
  port     = 3306
  manage_master_user_password = true   # 自动存 Secrets Manager

  multi_az               = var.env == "prod"  # staging 省成本
  backup_retention_period = 30
  backup_window          = "03:00-04:00"
  maintenance_window     = "Mon:04:00-Mon:05:00"
  deletion_protection    = var.env == "prod"

  vpc_security_group_ids = [aws_security_group.rds.id]
  db_subnet_group_name   = module.vpc.database_subnet_group_name

  performance_insights_enabled = true
  monitoring_interval         = 60
  enabled_cloudwatch_logs_exports = ["audit", "error", "slowquery"]
}

resource "aws_kms_key" "rds" {
  description             = "KMS for RDS encryption"
  deletion_window_in_days = 30
  enable_key_rotation     = true
}

# ── ElastiCache Redis (risk-stack + 缓存) ──
resource "aws_elasticache_replication_group" "redis" {
  replication_group_id = "pp-${var.env}-redis"
  description          = "Payment platform Redis"
  node_type            = var.env == "prod" ? "cache.r7g.large" : "cache.t4g.small"
  num_cache_clusters   = var.env == "prod" ? 3 : 2
  port                 = 6379
  parameter_group_name = "default.redis7"
  engine_version       = "7.1"

  automatic_failover_enabled = true
  multi_az_enabled           = var.env == "prod"
  at_rest_encryption_enabled = true
  transit_encryption_enabled = true

  subnet_group_name = aws_elasticache_subnet_group.redis.name
  security_group_ids = [aws_security_group.redis.id]
}

resource "aws_elasticache_subnet_group" "redis" {
  name       = "pp-${var.env}-redis"
  subnet_ids = module.vpc.private_subnets
}

# ── MSK Kafka (outbox publisher + reconplatform CDC) ──
resource "aws_msk_cluster" "kafka" {
  cluster_name           = "pp-${var.env}-kafka"
  kafka_version          = "3.6.0"
  number_of_broker_nodes = 3

  broker_node_group_info {
    instance_type   = var.env == "prod" ? "kafka.m7g.large" : "kafka.t3.small"
    client_subnets  = module.vpc.private_subnets
    security_groups = [aws_security_group.kafka.id]

    storage_info {
      ebs_storage_info {
        volume_size = 100
      }
    }
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = aws_kms_key.rds.arn
    encryption_in_transit {
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.kafka.name
      }
    }
  }
}

resource "aws_cloudwatch_log_group" "kafka" {
  name              = "/aws/msk/pp-${var.env}"
  retention_in_days = 30
}

# ── DNS + ACM ──
data "aws_route53_zone" "main" {
  name = var.dns_zone
}

resource "aws_acm_certificate" "wildcard" {
  domain_name               = "*.${var.dns_zone}"
  subject_alternative_names = [var.dns_zone]
  validation_method         = "DNS"

  lifecycle { create_before_destroy = true }
}

# ── 安全组 ──
resource "aws_security_group" "rds" {
  name        = "pp-${var.env}-rds"
  description = "RDS MySQL"
  vpc_id      = module.vpc.vpc_id

  ingress {
    from_port       = 3306
    to_port         = 3306
    protocol        = "tcp"
    security_groups = [module.eks.cluster_primary_security_group_id]
  }
}

resource "aws_security_group" "redis" {
  name        = "pp-${var.env}-redis"
  vpc_id      = module.vpc.vpc_id
  ingress {
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [module.eks.cluster_primary_security_group_id]
  }
}

resource "aws_security_group" "kafka" {
  name        = "pp-${var.env}-kafka"
  vpc_id      = module.vpc.vpc_id
  ingress {
    from_port       = 9092
    to_port         = 9094
    protocol        = "tcp"
    security_groups = [module.eks.cluster_primary_security_group_id]
  }
}

# ── 输出 ──
output "eks_cluster_endpoint" { value = module.eks.cluster_endpoint }
output "rds_endpoint"         { value = module.mysql.db_instance_endpoint }
output "redis_endpoint"       { value = aws_elasticache_replication_group.redis.primary_endpoint_address }
output "kafka_bootstrap_brokers" { value = aws_msk_cluster.kafka.bootstrap_brokers_tls }
output "kubectl_config_cmd" {
  value = "aws eks update-kubeconfig --name ${module.eks.cluster_name} --region ${var.region}"
}
