# terraform-provider-stackorder

Terraform and OpenTofu provider for [Stackorder](https://github.com/stackorder/stackorder): read the outputs another stack shares without reading its state.

## Status

The provider is under development and not yet published to a registry. It will offer `stackorder_outputs` as a data source and as an ephemeral resource: name a stack and receive the root-module outputs its producer shares through the Stackorder server, so the consumer needs no access to the producer's state.

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

## Development

Until Stackorder tags the release that carries the `api/v1` stack outputs types, `go.mod` requires `github.com/stackorder/stackorder` v0.3.0 as a placeholder, and the provider builds only in a Go workspace that also uses a Stackorder checkout with those types. Create the workspace in the repository root, naming the path of that checkout; git ignores `go.work` and `go.work.sum`:

```shell
go work init . ../stackorder
```

| Target | What it does |
|---|---|
| `make build` | Builds `terraform-provider-stackorder` in the repository root |
| `make test` | Runs the unit tests with the race detector |
| `make testacc` | Runs the acceptance tests (`TF_ACC=1`) with the binary at `TF_ACC_TERRAFORM_PATH`, else `terraform` on `PATH`, else a downloaded Terraform |
| `make lint` | Checks formatting, runs `go vet` and golangci-lint |
| `make fmt` | Formats the Go sources |
| `make generate` | Runs `go generate` |
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
