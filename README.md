# terraform-provider-stackorder

Terraform and OpenTofu provider for [Stackorder](https://github.com/stackorder/stackorder): read the outputs another stack shares without reading its state.

## Status

The provider is under development and not yet published to a registry. It offers `stackorder_outputs` as a data source and as an ephemeral resource: name a stack and receive the root-module outputs its producer shares through the Stackorder server, so the consumer needs no access to the producer's state.

## Documentation

The registry documentation lives in [`docs/`](docs), in the [layout the Terraform Registry reads](https://developer.hashicorp.com/terraform/registry/providers/docs) from each release tag:

| Page | Source |
|---|---|
| [`docs/index.md`](docs/index.md): the provider, sharing outputs, credentials, requirements and the provider configuration | [`templates/index.md.tmpl`](templates/index.md.tmpl), [`examples/provider/`](examples/provider) |
| [`docs/data-sources/outputs.md`](docs/data-sources/outputs.md): the `stackorder_outputs` data source | [`templates/data-sources/outputs.md.tmpl`](templates/data-sources/outputs.md.tmpl), [`examples/data-sources/stackorder_outputs/`](examples/data-sources/stackorder_outputs) |
| [`docs/ephemeral-resources/outputs.md`](docs/ephemeral-resources/outputs.md): the `stackorder_outputs` ephemeral resource | [`templates/ephemeral-resources/outputs.md.tmpl`](templates/ephemeral-resources/outputs.md.tmpl), [`examples/ephemeral-resources/stackorder_outputs/`](examples/ephemeral-resources/stackorder_outputs) |

[tfplugindocs](https://github.com/hashicorp/terraform-plugin-docs) generates every page from its template, its example files and the descriptions in the provider's schema, so `docs/` is never edited by hand: change the source and run `make generate`, then commit `docs/` with the change. Page and template names leave out the `stackorder_` prefix, as the registry expects; example directories keep it.

## Requirements

- Terraform 1.10 or later, or OpenTofu 1.11 or later, for the `stackorder_outputs` ephemeral resource.
- The data source works with any Terraform or OpenTofu release that supports plugin protocol 6 (Terraform 1.0 and later, every OpenTofu release).
- Go 1.26 or later to build the provider.

## Configuration

```hcl
provider "stackorder" {
  server_url = "https://stackorder.acme.example"
}
```

Inside a Stackorder plan, apply or drift job the block can stay empty: the Stackorder CLI sets `STACKORDER_SERVER_URL`, `STACKORDER_REPOSITORY`, `STACKORDER_RUN_ID` and `STACKORDER_STACK` for Terraform, and the job's own GitHub Actions identity authenticates the reads. Stackorder CLI releases up to 0.3.0 set `STACKORDER_STACK` but not the other three, so a job needs a later release to read outputs with its own identity.

Every attribute is optional and falls back to an environment variable. An attribute set to an empty string counts as unset.

| Attribute | Environment fallback | Meaning |
|---|---|---|
| `server_url` | `STACKORDER_SERVER_URL` | Base URL of the Stackorder server. One of the two is required. |
| `api_key` | `STACKORDER_API_KEY` | Automation API key (`sk_…`), used only where no GitHub Actions ID token can be requested. Sensitive. |
| `repository` | `STACKORDER_REPOSITORY`, then `GITHUB_REPOSITORY` | Repository of the stacks being planned, as `owner/repo`: the repository a stack named without an `owner/repo//` prefix belongs to. |
| `oidc_audience` | `STACKORDER_OIDC_AUDIENCE` | Audience of the GitHub Actions ID tokens, which must be one the server accepts. Defaults to the server URL (`server_url` or `STACKORDER_SERVER_URL`) as configured, trailing slash included. |

### Credentials

Where Terraform runs decides which credentials the provider uses:

- **Job token, inside Stackorder jobs.** Whenever the job can request GitHub Actions ID tokens (`permissions: id-token: write`, which the Stackorder reusable workflows grant), the provider requests a fresh ID token for every request to the server. The server accepts these only from a Stackorder plan, apply or drift job and checks the reading stack against the producer's readers, so the provider also needs `STACKORDER_RUN_ID` and `STACKORDER_STACK`, which the Stackorder CLI sets. Without them the provider refuses to configure. A configured API key is ignored in this case, so an admin key never silently replaces the job's own identity. A GitHub Actions job outside Stackorder that grants `id-token: write`, for example to assume a cloud role, therefore cannot use the provider.
- **API key, elsewhere.** Where no ID token can be requested, such as on a workstation, the provider sends the API key from `api_key` or `STACKORDER_API_KEY`. API keys are admin keys of the Stackorder server and are not bound to the readers a producer lists: keep them out of CI, and prefer `STACKORDER_API_KEY` over writing the key into configuration.

## Usage

A stack shares its outputs only with the stacks its producer lists under `outputs.readers` on its default branch, in the stack's `.stackorder.yaml` or in `stackorder.yaml` for every stack of its repository. A reader is a stack key or pattern of the same repository, or `owner/repo//pattern` for stacks of another repository of the same Stackorder installation. Sensitive outputs stay withheld unless the producer also sets `outputs.sensitive: share`:

```yaml
outputs:
  readers:
    - stacks/prod/eks
    - acme/platform//clusters/**
  sensitive: share
```

A reading stack then names the producer with the `stackorder_outputs` data source:

```hcl
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
```

`stack` takes `path` or `path:instance` for a stack of the provider's `repository`, or `owner/repo//key` for a stack of another repository. `values` holds every output the producer serves and is sensitive as a whole; `nonsensitive_values` holds the outputs not marked sensitive, and `id` the qualified key `owner/repo//key`. The data source warns about the sensitive outputs the producer withholds.

The server serves outputs once its operator enables them with `STACKORDER_STATE_BUCKETS` and an apply or drift job of the producer has reported where its state lives. Values read by a data source are stored in the plan file, which Stackorder uploads as a workflow artifact, and in state. To read sensitive values without storing them, use `ephemeral "stackorder_outputs"`, which Terraform never writes to the plan file or state:

```hcl
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
```

The ephemeral resource takes the same `stack` and returns the same `values` and `nonsensitive_values`, without `id`, and warns about withheld outputs in the same way. Its result, `nonsensitive_values` included, can only be referenced where Terraform accepts ephemeral values: write-only arguments such as `secret_string_wo` (Terraform 1.11 or later), provider and provisioner blocks, locals, ephemeral input variables, ephemeral outputs of child modules and other ephemeral resources. Terraform reads the outputs again in every plan and apply that needs them.

## Development

The provider builds from the published Stackorder module that `go.mod` requires, with no Stackorder checkout or Go workspace. `make test` runs the unit tests and `make testacc` the acceptance tests:

| Target | What it does |
|---|---|
| `make build` | Builds `terraform-provider-stackorder` in the repository root |
| `make test` | Runs the unit tests with the race detector |
| `make testacc` | Runs the acceptance tests (`TF_ACC=1`) with the binary at `TF_ACC_TERRAFORM_PATH`, else `terraform` on `PATH`, else a downloaded Terraform |
| `make lint` | Checks formatting, runs `go vet` and golangci-lint |
| `make fmt` | Formats the Go sources |
| `make generate` | Formats `examples/` with the `terraform` on `PATH`, then regenerates `docs/` from `templates/`, `examples/` and the provider schema with tfplugindocs, which builds the provider and reads its schema with Terraform 1.14.4, downloaded from releases.hashicorp.com on every run whatever `terraform` is on `PATH` |
| `make docs-check` | Runs `make generate` and fails when `docs/` or `examples/` then differs from the last commit, staged changes included, or `docs/` has a page git does not track |
| `make tidy` | Tidies `go.mod` and `go.sum` |

The acceptance tests never pick up `tofu` from `PATH`. To run them with OpenTofu, name the binary and OpenTofu's registry host:

```shell
TF_ACC_TERRAFORM_PATH="$(command -v tofu)" TF_ACC_PROVIDER_HOST=registry.opentofu.org make testacc
```

To try a local build, install it with `go install .` (it lands in `$(go env GOPATH)/bin`, usually `~/go/bin`, unless `GOBIN` is set) or build it with `make build`, then point Terraform at that directory with a CLI configuration file:

```hcl
provider_installation {
  dev_overrides {
    "stackorder/stackorder" = "/Users/you/go/bin"
  }
  direct {}
}
```

The path must be absolute: use your `~/go/bin`, or this repository's directory after `make build`. Save the block as `dev.tfrc` in the repository root, where git ignores `*.tfrc`, and select it for the current shell only by running this from the repository root:

```shell
export TF_CLI_CONFIG_FILE="$PWD/dev.tfrc"
```

With the override in place, `plan` and `apply` use the local binary and skip version and checksum checks for it. Running `init` is not needed for this provider and can fail while it is unpublished. OpenTofu reads the same `dev_overrides` block.

`go run . -debug` starts the provider in debug mode and prints a `TF_REATTACH_PROVIDERS` value; export it in the shell that runs `terraform plan` to use that process, for example under a debugger.
