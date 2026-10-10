package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderServesProtocol6(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "local build", version: "dev"},
		{name: "test build", version: "test"},
		{name: "release", version: "0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(tt.version)()

			var meta provider.MetadataResponse
			p.Metadata(t.Context(), provider.MetadataRequest{}, &meta)
			assert.Equal(t, "stackorder", meta.TypeName)
			assert.Equal(t, tt.version, meta.Version)

			server, err := providerserver.NewProtocol6WithError(p)()
			require.NoError(t, err)

			resp, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.Empty(t, resp.Diagnostics)
			require.NotNil(t, resp.Provider)
			require.NotNil(t, resp.Provider.Block)
			assert.Empty(t, resp.Provider.Block.Attributes)
			assert.Empty(t, resp.Provider.Block.BlockTypes)
			assert.Empty(t, resp.Provider.Block.Description)
			assert.Empty(t, resp.DataSourceSchemas)
			assert.Empty(t, resp.EphemeralResourceSchemas)
			assert.Empty(t, resp.ResourceSchemas)
		})
	}
}
