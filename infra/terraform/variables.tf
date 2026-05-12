variable "region" {
  type    = string
  default = "us-east-1"
}

variable "env" {
  type        = string
  description = "staging / prod"
  validation {
    condition     = contains(["staging", "prod", "dev"], var.env)
    error_message = "env must be staging / prod / dev"
  }
}

variable "dns_zone" {
  type        = string
  description = "已在 Route53 注册的 zone, eg 'payment.example.com'"
}
