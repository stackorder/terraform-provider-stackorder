// Package provider implements the stackorder provider on the Terraform Plugin Framework.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
)

const (
	envServerURL        = "STACKORDER_SERVER_URL"
	envRepository       = "STACKORDER_REPOSITORY"
	envGitHubRepository = "GITHUB_REPOSITORY"
	envRunID            = "STACKORDER_RUN_ID"
	envStack            = "STACKORDER_STACK"
)

var (
	_ provider.Provider                       = (*stackorderProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*stackorderProvider)(nil)
)

type stackorderProvider struct {
	version string
}

type providerModel struct {
	ServerURL    types.String `tfsdk:"server_url"`
	APIKey       types.String `tfsdk:"api_key"`
	Repository   types.String `tfsdk:"repository"`
	OIDCAudience types.String `tfsdk:"oidc_audience"`
}

type providerData struct {
	// Client reads stack outputs from the configured server.
	Client *client.Client
	// ServerHost is the host of the server URL, the only part of it that
	// diagnostics name.
	ServerHost string
	// IDTokens reports whether the provider authenticates with GitHub
	// Actions ID tokens rather than an API key.
	IDTokens bool
	// Repository is the "owner/repo" of the stacks being planned, empty when
	// no setting names it.
	Repository string
	// RunID is the Stackorder run of the job Terraform runs in, empty outside
	// a Stackorder job.
	RunID string
	// Reader is the key of the stack the job plans or applies, empty outside
	// a Stackorder job.
	Reader string
}

// New returns a constructor of the stackorder provider reporting version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &stackorderProvider{version: version}
	}
}

func (p *stackorderProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "stackorder"
	resp.Version = p.version
}

func (p *stackorderProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the outputs that stacks share through a [Stackorder](https://github.com/stackorder/stackorder) server, so a stack can use another stack's outputs without access to its state.",
		Attributes: map[string]schema.Attribute{
			"server_url": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Base URL of the Stackorder server, such as `https://stackorder.acme.example`. Defaults to the `" + envServerURL + "` environment variable, which the Stackorder CLI sets for Terraform in its jobs. One of the two is required.",
			},
			"api_key": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Automation API key (`sk_…`) the provider authenticates with outside GitHub Actions. Defaults to the `" + client.EnvAPIKey + "` environment variable. Ignored whenever the job can request GitHub Actions ID tokens, which take precedence. API keys are admin keys of the server: keep them out of CI.",
			},
			"repository": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Repository of the stacks being planned, as `owner/repo`, which a stack named without an `owner/repo//` prefix belongs to. Defaults to the `" + envRepository + "` environment variable, which the Stackorder CLI sets, else `" + envGitHubRepository + "`.",
			},
			"oidc_audience": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Audience of the GitHub Actions ID tokens the provider sends, which must be one the server accepts. Defaults to the `" + client.EnvOIDCAudience + "` environment variable, else the server URL (`server_url` or `" + envServerURL + "`) as configured, trailing slash included.",
			},
		},
	}
}

func (p *stackorderProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, a := range []struct {
		name, label, env string
		value            types.String
	}{
		{name: "server_url", label: "server URL", env: envServerURL, value: cfg.ServerURL},
		{name: "api_key", label: "API key", env: client.EnvAPIKey, value: cfg.APIKey},
		{name: "repository", label: "repository", env: envRepository, value: cfg.Repository},
		{name: "oidc_audience", label: "OIDC audience", env: client.EnvOIDCAudience, value: cfg.OIDCAudience},
	} {
		if a.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(a.name), "Unknown Stackorder "+a.label,
				fmt.Sprintf("The provider cannot be configured while %s is unknown. Set it to a value known at plan time, apply the resources it depends on first, or leave it unset and set %s instead.", a.name, a.env))
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	serverURL, serverURLSource := strings.TrimSpace(cfg.ServerURL.ValueString()), "server_url"
	if serverURL == "" {
		serverURL, serverURLSource = strings.TrimSpace(os.Getenv(envServerURL)), envServerURL
	}
	if serverURL == "" {
		resp.Diagnostics.AddAttributeError(path.Root("server_url"), "Missing Stackorder server URL",
			"Set the provider's server_url or "+envServerURL+" to the base URL of the Stackorder server. Inside a Stackorder job the Stackorder CLI sets "+envServerURL+".")
		return
	}
	tokens, err := client.NewTokenSource(client.Credentials{
		ServerURL:    serverURL,
		OIDCAudience: cfg.OIDCAudience.ValueString(),
		APIKey:       cfg.APIKey.ValueString(),
	})
	switch {
	case errors.Is(err, client.ErrNoCredentials):
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Missing Stackorder credentials", err.Error())
		return
	case err != nil:
		resp.Diagnostics.AddError("Invalid Stackorder credentials", err.Error())
		return
	}

	data := &providerData{
		Repository: firstNonEmpty(cfg.Repository.ValueString(), os.Getenv(envRepository), os.Getenv(envGitHubRepository)),
		RunID:      strings.TrimSpace(os.Getenv(envRunID)),
		Reader:     strings.TrimSpace(os.Getenv(envStack)),
		ServerHost: hostOf(serverURL),
		IDTokens:   client.ActionsIDTokensAvailable(),
	}
	if data.IDTokens {
		if missing := missingJobVariables(data); missing != "" {
			resp.Diagnostics.AddError("Not inside a Stackorder job", fmt.Sprintf(
				"%s. This job can request GitHub Actions ID tokens, so the provider authenticates with them, and reads with an ID token work only inside a Stackorder plan, apply or drift job, where the Stackorder CLI sets %s and %s for Terraform. Outside one, configure api_key or %s in an environment without ID tokens, since they take precedence over an API key.",
				missing, envRunID, envStack, client.EnvAPIKey))
		}
	}
	data.Client, err = client.New(serverURL, tokens, client.WithVersion(p.version))
	if err != nil {
		detail := fmt.Sprintf("The URL in %s is not usable: %s.", serverURLSource, err)
		if serverURLSource == envServerURL {
			resp.Diagnostics.AddError("Invalid Stackorder server URL", detail)
		} else {
			resp.Diagnostics.AddAttributeError(path.Root("server_url"), "Invalid Stackorder server URL", detail)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.DataSourceData = data
	resp.EphemeralResourceData = data
}

func (p *stackorderProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewOutputsDataSource}
}

func (p *stackorderProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{NewOutputsEphemeralResource}
}

func (p *stackorderProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{}
}

func missingJobVariables(data *providerData) string {
	switch {
	case data.RunID == "" && data.Reader == "":
		return envRunID + " and " + envStack + " are not set"
	case data.RunID == "":
		return envRunID + " is not set"
	case data.Reader == "":
		return envStack + " is not set"
	}
	return ""
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
