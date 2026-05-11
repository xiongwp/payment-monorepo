# Route53 health-check + weighted failover Terraform.
#
# 部署:
#   cd deploy/multi-region
#   terraform init
#   terraform apply -var "primary_region=us-east-1" -var "dr_region=eu-west-1"
#
# 行为:
#   - Primary 健康 (30s × 3 = 1.5min) → 100% 流量到 primary
#   - Primary 健康检查失败 → 自动 set primary weight=0, 流量切到 DR
#   - DNS TTL 60s, RTO ~5min (1.5min 检测 + 1min DNS + 2min DR scale)

variable "primary_region" { default = "us-east-1" }
variable "dr_region"      { default = "eu-west-1" }
variable "domain"         { default = "api.payment.example.com" }
variable "zone_id"        { default = "Z123ABCXYZ" }

# ─── 健康检查 ──────────────────────────────────────────────
resource "aws_route53_health_check" "primary" {
  fqdn                 = "api-${var.primary_region}.payment.example.com"
  port                 = 443
  type                 = "HTTPS"
  resource_path        = "/healthz"
  failure_threshold    = 3
  request_interval     = 30
  enable_sni           = true
  measure_latency      = true
  regions              = ["us-east-1", "us-west-1", "eu-west-1"]
  tags = { Name = "payment-primary-healthcheck" }
}

resource "aws_route53_health_check" "dr" {
  fqdn                 = "api-${var.dr_region}.payment.example.com"
  port                 = 443
  type                 = "HTTPS"
  resource_path        = "/healthz"
  failure_threshold    = 3
  request_interval     = 30
  enable_sni           = true
  tags = { Name = "payment-dr-healthcheck" }
}

# ─── Failover routing record ─────────────────────────────
# Primary record — health check 通过时 active
resource "aws_route53_record" "primary" {
  zone_id        = var.zone_id
  name           = var.domain
  type           = "A"
  set_identifier = "primary"
  health_check_id = aws_route53_health_check.primary.id
  failover_routing_policy { type = "PRIMARY" }
  alias {
    name                   = "primary-${var.primary_region}-elb.amazonaws.com"
    zone_id                = "ZHJTQHJTQHJTQ"   # ELB zone id (region-specific)
    evaluate_target_health = true
  }
}

# Secondary record — primary 健康检查失败时自动接管
resource "aws_route53_record" "dr" {
  zone_id        = var.zone_id
  name           = var.domain
  type           = "A"
  set_identifier = "dr"
  failover_routing_policy { type = "SECONDARY" }
  alias {
    name                   = "dr-${var.dr_region}-elb.amazonaws.com"
    zone_id                = "ZHJTQHJTQHJTQEU"
    evaluate_target_health = true
  }
}

# ─── Health check 失败告警 → SNS / PagerDuty ────────────
resource "aws_cloudwatch_metric_alarm" "primary_unhealthy" {
  alarm_name          = "payment-primary-unhealthy"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 1
  metric_name         = "HealthCheckStatus"
  namespace           = "AWS/Route53"
  period              = 60
  statistic           = "Minimum"
  threshold           = 1
  alarm_description   = "payment primary region health check failing"
  alarm_actions       = ["arn:aws:sns:us-east-1:CHANGE:payment-page-oncall"]
  dimensions = { HealthCheckId = aws_route53_health_check.primary.id }
}

output "primary_record_name" { value = aws_route53_record.primary.name }
output "dr_record_name"      { value = aws_route53_record.dr.name }
