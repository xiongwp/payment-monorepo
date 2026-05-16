env    = "prod"
region = "us-east-1"

vpc_cidr = "10.0.0.0/16"
azs      = ["us-east-1a", "us-east-1b", "us-east-1c"]

public_subnets  = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
private_subnets = ["10.0.11.0/24", "10.0.12.0/24", "10.0.13.0/24"]

# 生产 6 通用 + 3 数据 + 3 PCI;HPA 在此基础上扩缩.
eks_node_general_desired = 6
eks_node_data_desired    = 3
