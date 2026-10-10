package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ ephemeral.EphemeralResource              = (*outputsEphemeralResource)(nil)
	_ ephemeral.EphemeralResourceWithConfigure = (*outputsEphemeralResource)(nil)
)

type outputsEphemeralResource struct {
	data *providerData
}

type outputsEphemeralModel struct {
	Stack              types.String  `tfsdk:"stack"`
	Values             types.Dynamic `tfsdk:"values"`
	NonsensitiveValues types.Dynamic `tfsdk:"nonsensitive_values"`
}

// NewOutputsEphemeralResource returns the stackorder_outputs ephemeral
// resource, which reads the outputs a stack shares through the Stackorder
// server as the data source does, but which Terraform never writes to the
// plan file or state. It implements neither Renew nor Close: the outputs
// are a copy of values in the producer's state, not a lease or a session on
// the server, so they never expire and leave nothing to release.
func NewOutputsEphemeralResource() ephemeral.EphemeralResource {
	return &outputsEphemeralResource{}
}

// Metadata names the ephemeral resource stackorder_outputs.
func (r *outputsEphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_outputs"
}

// Schema describes the stack the ephemeral resource names and the values it
// reads, as the data source does but without an id.
func (r *outputsEphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads the root-module outputs a stack shares through the Stackorder server, as the `stackorder_outputs` data source does, but Terraform never writes them to the plan file or to state. " +
			"This is the form to use for sensitive outputs. It requires Terraform 1.10 or later, or OpenTofu 1.11 or later.\n\n" +
			sharingDescription("ephemeral resource") + "\n\n" +
			"Like every ephemeral value, the result can only be referenced where Terraform accepts ephemeral values: write-only arguments of managed resources (Terraform 1.11 or later), provider and provisioner blocks, locals, ephemeral input variables, ephemeral outputs of child modules and other ephemeral resources. " +
			"Terraform reads the outputs again in every plan and apply that needs them.",
		Attributes: map[string]schema.Attribute{
			"stack": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: stackDescription,
			},
			"values": schema.DynamicAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: valuesDescription,
			},
			"nonsensitive_values": schema.DynamicAttribute{
				Computed: true,
				MarkdownDescription: "The object of `values` without the outputs marked sensitive. " +
					"Terraform does not treat it as sensitive, but it is ephemeral like the rest of the result.",
			},
		},
	}
}

// Configure keeps the settings of the configured provider for Open.
func (r *outputsEphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	data, problem := providerDataOf(req.ProviderData, "ephemeral resource")
	if problem != nil {
		resp.Diagnostics.Append(problem)
		return
	}
	r.data = data
}

// Open requests the stack's outputs from the Stackorder server, exactly as
// the data source's Read does, and returns them with the configured stack.
func (r *outputsEphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var model outputsEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}
	read, diags := readStackOutputs(ctx, r.data, model.Stack.ValueString())
	resp.Diagnostics.Append(diags...)
	if diags.HasError() {
		return
	}
	model.Values = read.values
	model.NonsensitiveValues = read.nonsensitiveValues
	resp.Diagnostics.Append(resp.Result.Set(ctx, &model)...)
}
