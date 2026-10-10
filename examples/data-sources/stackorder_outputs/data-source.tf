data "stackorder_outputs" "vpc" {
  stack = "stacks/prod/vpc"
}

resource "aws_security_group" "cluster" {
  name   = "eks-cluster"
  vpc_id = data.stackorder_outputs.vpc.nonsensitive_values.vpc_id
}

data "stackorder_outputs" "dns" {
  stack = "acme/platform//stacks/prod/dns"
}

output "zone_id" {
  value = data.stackorder_outputs.dns.nonsensitive_values.zone_id
}
