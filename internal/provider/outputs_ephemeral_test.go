package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/echoprovider"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openThroughServer(t *testing.T, stack string) (map[string]tftypes.Value, []*tfprotov6.Diagnostic) {
	t.Helper()
	server, schemas := configuredServer(t)
	es := schemas.EphemeralResourceSchemas[outputsTypeName]
	require.NotNil(t, es)
	config, err := tfprotov6.NewDynamicValue(es.ValueType(), nullObject(es.Block, map[string]tftypes.Value{"stack": str(stack)}))
	require.NoError(t, err)
	validated, err := server.ValidateEphemeralResourceConfig(t.Context(), &tfprotov6.ValidateEphemeralResourceConfigRequest{TypeName: outputsTypeName, Config: &config})
	require.NoError(t, err)
	require.Empty(t, validated.Diagnostics)

	opened, err := server.OpenEphemeralResource(t.Context(), &tfprotov6.OpenEphemeralResourceRequest{TypeName: outputsTypeName, Config: &config})
	require.NoError(t, err)
	assertNoSecretsServed(t, opened.Diagnostics)
	assert.True(t, opened.RenewAt.IsZero(), "renew at %s", opened.RenewAt)
	assert.Nil(t, opened.Deferred)
	return objectAttributes(t, opened.Result, es.ValueType()), opened.Diagnostics
}

func TestOutputsEphemeralSchema(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	require.NoError(t, err)
	schemas, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	es := schemas.EphemeralResourceSchemas[outputsTypeName]
	require.NotNil(t, es)
	ds := schemas.DataSourceSchemas[outputsTypeName]
	require.NotNil(t, ds)

	type flags struct{ required, computed, sensitive bool }
	got := map[string]flags{}
	types := map[string]tftypes.Type{}
	descriptions := map[string]string{}
	for _, a := range es.Block.Attributes {
		got[a.Name] = flags{required: a.Required, computed: a.Computed, sensitive: a.Sensitive}
		types[a.Name] = a.Type
		descriptions[a.Name] = a.Description
	}
	assert.Equal(t, map[string]flags{
		"stack":               {required: true},
		"values":              {computed: true, sensitive: true},
		"nonsensitive_values": {computed: true},
	}, got)
	assert.Equal(t, map[string]tftypes.Type{
		"stack":               tftypes.String,
		"values":              tftypes.DynamicPseudoType,
		"nonsensitive_values": tftypes.DynamicPseudoType,
	}, types)
	assert.Empty(t, es.Block.BlockTypes)
	for _, a := range ds.Block.Attributes {
		if a.Name == "stack" || a.Name == "values" {
			assert.Equal(t, a.Description, descriptions[a.Name], "description of %s", a.Name)
		}
	}
	assert.Contains(t, descriptions["nonsensitive_values"], "ephemeral")
	for _, want := range []string{"never writes them to the plan file or to state", "sensitive outputs", "Terraform 1.10", "OpenTofu 1.11", "outputs.readers", "outputs.sensitive: share", "the ephemeral resource warns", "default branch", "write-only arguments"} {
		assert.Contains(t, es.Block.Description, want)
	}
}

func TestOutputsEphemeralOpen(t *testing.T) {
	for _, tt := range outputsReadCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOutputsServer(t, map[string]served{tt.path: okWith(tt.body)})
			serverEnvironment(t, srv, tt.env)

			attrs, diags := openThroughServer(t, tt.stack)

			tt.assertRead(t, srv, attrs, diags)
			assert.Equal(t, []string{"nonsensitive_values", "stack", "values"}, slices.Sorted(maps.Keys(attrs)))
		})
	}
}

func TestOutputsEphemeralOpenErrors(t *testing.T) {
	for _, tt := range outputsReadErrorCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOutputsServer(t, tt.answers)
			serverEnvironment(t, srv, tt.env)

			attrs, diags := openThroughServer(t, tt.stack)

			tt.assertFailure(t, srv, attrs, diags)
			assert.NotContains(t, attrs, "id")
		})
	}
}

func TestOutputsConfigure(t *testing.T) {
	configured := &providerData{ServerHost: testServerHost, Repository: "acme/infra"}
	kinds := []struct {
		name      string
		configure func(t *testing.T, raw any) (*providerData, diag.Diagnostics)
	}{
		{
			name: "data source",
			configure: func(t *testing.T, raw any) (*providerData, diag.Diagnostics) {
				d := &outputsDataSource{}
				var resp datasource.ConfigureResponse
				d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: raw}, &resp)
				return d.data, resp.Diagnostics
			},
		},
		{
			name: "ephemeral resource",
			configure: func(t *testing.T, raw any) (*providerData, diag.Diagnostics) {
				r := &outputsEphemeralResource{}
				var resp ephemeral.ConfigureResponse
				r.Configure(t.Context(), ephemeral.ConfigureRequest{ProviderData: raw}, &resp)
				return r.data, resp.Diagnostics
			},
		},
	}
	tests := []struct {
		name       string
		raw        any
		want       *providerData
		wantDetail string
	}{
		{name: "before the provider is configured"},
		{name: "configured provider", raw: configured, want: configured},
		{name: "provider data of another type", raw: "acme", wantDetail: "received provider data of type string"},
	}
	for _, kind := range kinds {
		for _, tt := range tests {
			t.Run(kind.name+"/"+tt.name, func(t *testing.T) {
				got, diags := kind.configure(t, tt.raw)

				assert.True(t, got == tt.want, "provider data %+v", got)
				if tt.wantDetail == "" {
					assert.Empty(t, diags)
					return
				}
				require.Len(t, diags, 1)
				assert.Equal(t, diag.SeverityError, diags[0].Severity())
				assert.Equal(t, "Unexpected provider data", diags[0].Summary())
				assert.Contains(t, diags[0].Detail(), "The stackorder_outputs "+kind.name+" "+tt.wantDetail)
			})
		}
	}
}

func testAccProtoV6ProviderFactoriesWithEcho() map[string]func() (tfprotov6.ProviderServer, error) {
	factories := testAccProtoV6ProviderFactories()
	factories["echo"] = echoprovider.NewProviderServer()
	return factories
}

func ephemeralResourcesSupported() tfversion.TerraformVersionCheck {
	// tofu version -json reports OpenTofu's own version, and OpenTofu has ephemeral resources only from 1.11.
	if strings.Contains(filepath.Base(os.Getenv("TF_ACC_TERRAFORM_PATH")), "tofu") || os.Getenv("TF_ACC_PROVIDER_HOST") == "registry.opentofu.org" {
		return tfversion.SkipBelow(tfversion.Version1_11_0)
	}
	return tfversion.SkipBelow(tfversion.Version1_10_0)
}

const echoAddress = "echo.x"

func holdsOnlyEcho(m *tfjson.StateModule) error {
	if m == nil {
		return fmt.Errorf("no root module, want only %s", echoAddress)
	}
	addresses := make([]string, 0, len(m.Resources))
	for _, r := range m.Resources {
		addresses = append(addresses, r.Address)
	}
	if !slices.Equal(addresses, []string{echoAddress}) || len(m.ChildModules) > 0 {
		return fmt.Errorf("the root module holds %v and %d child modules, want only %s", addresses, len(m.ChildModules), echoAddress)
	}
	return nil
}

type stateHoldingOnlyEcho struct{ secret string }

func (c stateHoldingOnlyEcho) CheckState(_ context.Context, req statecheck.CheckStateRequest, resp *statecheck.CheckStateResponse) {
	if req.State == nil || req.State.Values == nil {
		resp.Error = fmt.Errorf("no state to check")
		return
	}
	root := req.State.Values.RootModule
	if err := holdsOnlyEcho(root); err != nil {
		resp.Error = err
		return
	}
	all, err := json.Marshal(req.State)
	if err != nil {
		resp.Error = err
		return
	}
	echo, err := json.Marshal(root.Resources[0])
	if err != nil {
		resp.Error = err
		return
	}
	if bytes.Count(all, []byte(c.secret)) != bytes.Count(echo, []byte(c.secret)) {
		resp.Error = fmt.Errorf("state holds the value of a sensitive output outside %s", echoAddress)
	}
}

type planHoldingOnlyEcho struct{ secret string }

func (c planHoldingOnlyEcho) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	raw, err := json.Marshal(req.Plan)
	if err != nil {
		resp.Error = err
		return
	}
	if bytes.Contains(raw, []byte(c.secret)) {
		resp.Error = fmt.Errorf("the plan holds the value of a sensitive output")
		return
	}
	changed := make([]string, 0, len(req.Plan.ResourceChanges))
	for _, rc := range req.Plan.ResourceChanges {
		changed = append(changed, rc.Address)
	}
	if !slices.Equal(changed, []string{echoAddress}) {
		resp.Error = fmt.Errorf("the plan changes %v, want only %s", changed, echoAddress)
		return
	}
	if req.Plan.PlannedValues == nil {
		resp.Error = fmt.Errorf("the plan has no planned values")
		return
	}
	resp.Error = holdsOnlyEcho(req.Plan.PlannedValues.RootModule)
}

func TestAccOutputsEphemeral(t *testing.T) {
	srv := newOutputsServer(t, map[string]served{vpcOutputsPath: okWith(sharingSensitive(t))})
	serverEnvironment(t, srv, map[string]string{"GITHUB_REPOSITORY": "acme/infra"})

	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   []tfversion.TerraformVersionCheck{ephemeralResourcesSupported()},
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactoriesWithEcho(),
		Steps: []resource.TestStep{{
			Config: `
ephemeral "stackorder_outputs" "vpc" {
  stack = "stacks/prod/vpc"
}

provider "echo" {
  data = ephemeral.stackorder_outputs.vpc
}

resource "echo" "x" {}
`,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{planHoldingOnlyEcho{secret: pskCanary}},
			},
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue(echoAddress, tfjsonpath.New("data"), knownvalue.ObjectExact(map[string]knownvalue.Check{
					"stack":               knownvalue.StringExact(vpcStack),
					"values":              vpcCheck(map[string]knownvalue.Check{"vpn_preshared_key": knownvalue.StringExact(pskCanary)}),
					"nonsensitive_values": vpcCheck(nil),
				})),
				stateHoldingOnlyEcho{secret: pskCanary},
			},
		}},
	})

	got := srv.requests()
	require.NotEmpty(t, got)
	for _, r := range got {
		assert.Equal(t, vpcOutputsPath, r.path)
	}
}
