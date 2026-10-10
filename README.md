# terraform-provider-stackorder

Terraform and OpenTofu provider for [Stackorder](https://github.com/stackorder/stackorder): read the outputs another stack shares without reading its state.

## Status

The provider is under development and not yet published to a registry. It will offer `stackorder_outputs` as a data source and as an ephemeral resource: name a stack and receive the root-module outputs its producer shares through the Stackorder server, so the consumer needs no access to the producer's state.

## Requirements

- Terraform 1.10 or later, or OpenTofu 1.11 or later, for the `stackorder_outputs` ephemeral resource.
- The data source works with any Terraform or OpenTofu release that supports plugin protocol 6 (Terraform 1.0 and later, every OpenTofu release).
- Go 1.26 or later to build the provider.

## Development

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
