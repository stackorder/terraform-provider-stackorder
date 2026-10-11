# Changelog

All notable changes to this provider are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and releases follow [Semantic Versioning](https://semver.org/). Each `vX.Y.Z` tag publishes signed archives for the Terraform and OpenTofu registries. A tag takes its release notes from the section of its version, so a pre-release tag such as `v0.1.0-rc.1` needs a section of its own.

## [Unreleased]

## [0.1.0] - 2026-10-11

It needs Stackorder 0.4.0 or later on the server and in the CLI, the first release to serve shared outputs.

### Added

- The `stackorder` provider for Terraform and OpenTofu, at `registry.terraform.io/stackorder/stackorder` on plugin protocol 6. It reads the outputs another stack shares through the [Stackorder](https://github.com/stackorder/stackorder) server, so a stack needs no access to the state of the stacks it reads. `server_url`, `api_key`, `repository` and `oidc_audience` are optional and fall back to `STACKORDER_SERVER_URL`, `STACKORDER_API_KEY`, `STACKORDER_REPOSITORY` (then `GITHUB_REPOSITORY`) and `STACKORDER_OIDC_AUDIENCE`.
- The `stackorder_outputs` data source: `stack` names the producer as `path`, `path:instance` or `owner/repo//key`, `values` holds every output the producer serves and is sensitive, `nonsensitive_values` holds the outputs not marked sensitive, and a warning names the sensitive outputs the producer withholds. Each refusal of the server gets its own error, which says what to check or change.
- The `stackorder_outputs` ephemeral resource, with the same `stack`, `values` and `nonsensitive_values`, which Terraform never writes to the plan file or state. It needs Terraform 1.10 or OpenTofu 1.11 or later.
- Credentials: inside a Stackorder plan, apply or drift job, a fresh GitHub Actions ID token for every request, bound to the reading stack through `STACKORDER_RUN_ID` and `STACKORDER_STACK`; elsewhere, an API key from `api_key` or `STACKORDER_API_KEY`. Where an ID token can be requested, a configured API key is ignored, so an admin key never replaces the job's own identity.

[Unreleased]: https://github.com/stackorder/terraform-provider-stackorder/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/stackorder/terraform-provider-stackorder/releases/tag/v0.1.0
