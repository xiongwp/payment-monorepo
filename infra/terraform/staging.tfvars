env    = "staging"
region = "us-east-1"

vpc_cidr = "10.10.0.0/16"
azs      = ["us-east-1a", "us-east-1b", "us-east-1c"]

public_subnets  = ["10.10.1.0/24", "10.10.2.0/24", "10.10.3.0/24"]
private_subnets = ["10.10.11.0/24", "10.10.12.0/24", "10.10.13.0/24"]

eks_node_general_desired = 3
eks_node_data_desired    = 3
