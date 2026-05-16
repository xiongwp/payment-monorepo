# RDS Aurora MySQL 3.0 (MySQL 8.0 compatible) module
#
# 输出:
#   cluster_endpoint  写库端点
#   reader_endpoint   只读端点 (含所有 reader)
#   secret_arn        master password Secret Manager ARN

variable "env"                  { type = string }
variable "vpc_id"               { type = string }
variable "subnet_ids"           { type = list(string) }
variable "allowed_cidr_blocks"  { type = list(string) }

variable "cluster_identifier"   { type = string }
variable "database_name"        { type = string }
variable "master_username"      { type = string }
variable "instance_count"       { type = number default = 2 }
variable "instance_class"       { type = string default = "db.r6i.large" }
variable "backup_retention_days" { type = number default = 7 }
variable "enable_pitr"          { type = bool default = true }
variable "enable_iam_auth"      { type = bool default = true }

resource "random_password" "master" {
  length  = 32
  special = false
}

resource "aws_secretsmanager_secret" "master" {
  name = "rds/${var.cluster_identifier}/master"
}

resource "aws_secretsmanager_secret_version" "master" {
  secret_id = aws_secretsmanager_secret.master.id
  secret_string = jsonencode({
    username = var.master_username
    password = random_password.master.result
  })
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.cluster_identifier}-subnets"
  subnet_ids = var.subnet_ids
}

resource "aws_security_group" "this" {
  name   = "${var.cluster_identifier}-sg"
  vpc_id = var.vpc_id

  ingress {
    from_port   = 3306
    to_port     = 3306
    protocol    = "tcp"
    cidr_blocks = var.allowed_cidr_blocks
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_rds_cluster" "this" {
  cluster_identifier      = var.cluster_identifier
  engine                  = "aurora-mysql"
  engine_version          = "8.0.mysql_aurora.3.05.0"
  database_name           = var.database_name
  master_username         = var.master_username
  master_password         = random_password.master.result
  db_subnet_group_name    = aws_db_subnet_group.this.name
  vpc_security_group_ids  = [aws_security_group.this.id]

  backup_retention_period = var.backup_retention_days
  preferred_backup_window = "02:00-04:00"
  skip_final_snapshot     = false
  final_snapshot_identifier = "${var.cluster_identifier}-final"
  copy_tags_to_snapshot   = true

  enable_http_endpoint    = false   # Data API 不开,降低攻击面
  storage_encrypted       = true
  iam_database_authentication_enabled = var.enable_iam_auth

  enabled_cloudwatch_logs_exports = ["audit", "error", "general", "slowquery"]

  deletion_protection = true

  lifecycle {
    ignore_changes = [master_password]
  }
}

resource "aws_rds_cluster_instance" "this" {
  count                = var.instance_count
  identifier           = "${var.cluster_identifier}-${count.index}"
  cluster_identifier   = aws_rds_cluster.this.id
  instance_class       = var.instance_class
  engine               = "aurora-mysql"
  db_subnet_group_name = aws_db_subnet_group.this.name

  performance_insights_enabled    = true
  performance_insights_retention_period = 7
  monitoring_interval             = 30
  monitoring_role_arn             = aws_iam_role.rds_monitoring.arn

  publicly_accessible = false
}

resource "aws_iam_role" "rds_monitoring" {
  name = "${var.cluster_identifier}-monitoring"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = { Service = "monitoring.rds.amazonaws.com" }
      Action = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "rds_monitoring" {
  role       = aws_iam_role.rds_monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

output "cluster_endpoint" { value = aws_rds_cluster.this.endpoint }
output "reader_endpoint"  { value = aws_rds_cluster.this.reader_endpoint }
output "secret_arn"       { value = aws_secretsmanager_secret.master.arn }
output "cluster_id"       { value = aws_rds_cluster.this.id }
