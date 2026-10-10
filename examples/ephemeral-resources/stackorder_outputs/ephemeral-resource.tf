ephemeral "stackorder_outputs" "database" {
  stack = "stacks/prod/database"
}

resource "aws_secretsmanager_secret" "database" {
  name = "acme/prod/eks/database"
}

resource "aws_secretsmanager_secret_version" "database" {
  secret_id                = aws_secretsmanager_secret.database.id
  secret_string_wo         = ephemeral.stackorder_outputs.database.values.master_password
  secret_string_wo_version = 1
}
