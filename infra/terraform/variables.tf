variable "region" {
  type    = string
  default = "us-east-1"
}

variable "env" {
  type        = string
  description = "staging / prod / dev"
  validation {
    condition     = contains(["staging", "prod", "dev"], var.env)
    error_message = "env must be staging / prod / dev"
  }
}

variable "dns_zone" {
  type        = string
  description = "已在 Route53 注册的 zone, eg 'payment.example.com'"
  default     = ""
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.0.0.0/16"
}

variable "azs" {
  description = "Availability zones"
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

variable "public_subnets" {
  description = "Public subnet CIDRs"
  type        = list(string)
  default     = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
}

variable "private_subnets" {
  description = "Private subnet CIDRs"
  type        = list(string)
  default     = ["10.0.11.0/24", "10.0.12.0/24", "10.0.13.0/24"]
}

variable "eks_node_general_desired" {
  description = "Desired count for general node group"
  type        = number
  default     = 6
}

variable "eks_node_data_desired" {
  description = "Desired count for data node group"
  type        = number
  default     = 3
}
