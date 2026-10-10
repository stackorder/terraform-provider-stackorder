// Package provider implements the stackorder provider on the Terraform Plugin Framework.
package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

var (
	_ provider.Provider                       = (*stackorderProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*stackorderProvider)(nil)
)

type stackorderProvider struct {
	version string
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
	resp.Schema = schema.Schema{}
}

func (p *stackorderProvider) Configure(_ context.Context, _ provider.ConfigureRequest, _ *provider.ConfigureResponse) {
}

func (p *stackorderProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func (p *stackorderProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{}
}

func (p *stackorderProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{}
}
