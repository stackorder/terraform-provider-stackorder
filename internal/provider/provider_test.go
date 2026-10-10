package provider

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/stackorder/stackorder/api/v1"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
)

const (
	testAPIKey       = "sk_acme_automation"
	testRequestToken = "actions-request-token"
	testURLPassword  = "hunter2"
	testRunID        = "5b1e3f0a-8c2d-4c1b-9a51-0d7f3e2a9b11"
	testReader       = "stacks/prod/eks"
	testOutputsPath  = "/v1/repos/acme/network/stacks/stacks%2Fprod%2Fvpc/outputs"
)

type request struct {
	path      string
	query     url.Values
	auth      string
	userAgent string
}

type fakeStackorder struct {
	url string
	mu  sync.Mutex
	got []request
}

func newFakeStackorder(t *testing.T) *fakeStackorder {
	t.Helper()
	f := &fakeStackorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, request{
			path:      r.URL.EscapedPath(),
			query:     r.URL.Query(),
			auth:      r.Header.Get("Authorization"),
			userAgent: r.Header.Get("User-Agent"),
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v1.StackOutputs{Repo: "acme/network", Stack: "stacks/prod/vpc", Outputs: map[string]v1.StackOutput{}})
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeStackorder) read(t *testing.T, data *providerData) request {
	t.Helper()
	f.mu.Lock()
	before := len(f.got)
	f.mu.Unlock()
	out, err := data.Client.StackOutputs(t.Context(), "acme/network", "stacks/prod/vpc", data.RunID, data.Reader)
	require.NoError(t, err)
	assert.Equal(t, "stacks/prod/vpc", out.Stack)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.got, before+1)
	return f.got[before]
}

func fakeActions(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testRequestToken {
			http.Error(w, `{"message":"bad token request"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "id-token-for:" + r.URL.Query().Get("audience")})
	}))
	t.Cleanup(srv.Close)
	t.Setenv(client.EnvOIDCRequestURL, srv.URL+"/idtoken?api-version=2.0")
	t.Setenv(client.EnvOIDCRequestToken, testRequestToken)
}

func cleanEnvironment(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{
		envServerURL, envRepository, envGitHubRepository, envRunID, envStack,
		client.EnvAPIKey, client.EnvOIDCAudience, client.EnvOIDCRequestURL, client.EnvOIDCRequestToken,
	} {
		t.Setenv(name, "")
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

func known(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

func unknown() tftypes.Value { return tftypes.NewValue(tftypes.String, tftypes.UnknownValue) }

func providerSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp provider.SchemaResponse
	New("test")().Schema(t.Context(), provider.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	return resp.Schema
}

func configure(t *testing.T, config map[string]tftypes.Value) provider.ConfigureResponse {
	t.Helper()
	s := providerSchema(t)
	values := make(map[string]tftypes.Value, len(s.Attributes))
	for name := range s.Attributes {
		values[name] = tftypes.NewValue(tftypes.String, nil)
	}
	for name, v := range config {
		require.Contains(t, values, name)
		values[name] = v
	}
	var resp provider.ConfigureResponse
	New("test")().Configure(t.Context(), provider.ConfigureRequest{
		TerraformVersion: "1.16.5",
		Config:           tfsdk.Config{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(t.Context()), values)},
	}, &resp)
	assertNoSecrets(t, resp.Diagnostics)
	return resp
}

func configured(t *testing.T, resp provider.ConfigureResponse) *providerData {
	t.Helper()
	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)
	assert.Empty(t, resp.Diagnostics)
	data, ok := resp.DataSourceData.(*providerData)
	require.True(t, ok, "DataSourceData is %T", resp.DataSourceData)
	assert.Same(t, data, resp.EphemeralResourceData)
	require.NotNil(t, data.Client)
	return data
}

func requireError(t *testing.T, resp provider.ConfigureResponse, attribute, summary string) diag.Diagnostic {
	t.Helper()
	assert.Nil(t, resp.DataSourceData)
	assert.Nil(t, resp.EphemeralResourceData)
	require.Len(t, resp.Diagnostics, 1, "%v", resp.Diagnostics)
	d := resp.Diagnostics[0]
	assert.Equal(t, diag.SeverityError, d.Severity())
	assert.Equal(t, summary, d.Summary())
	assert.Equal(t, attribute, diagnosticPath(d))
	return d
}

func diagnosticPath(d diag.Diagnostic) string {
	if wp, ok := d.(diag.DiagnosticWithPath); ok {
		return wp.Path().String()
	}
	return ""
}

func assertNoSecrets(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	for _, d := range diags {
		for _, secret := range []string{testAPIKey, testRequestToken, testURLPassword} {
			assert.NotContains(t, d.Summary()+" "+d.Detail(), secret)
		}
	}
}

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
			attributes := map[string]bool{}
			for _, a := range resp.Provider.Block.Attributes {
				attributes[a.Name] = a.Sensitive
			}
			assert.Equal(t, map[string]bool{"server_url": false, "api_key": true, "repository": false, "oidc_audience": false}, attributes)
			assert.Empty(t, resp.Provider.Block.BlockTypes)
			assert.Equal(t, []string{"stackorder_outputs"}, slices.Sorted(maps.Keys(resp.DataSourceSchemas)))
			assert.Equal(t, []string{"stackorder_outputs"}, slices.Sorted(maps.Keys(resp.EphemeralResourceSchemas)))
			assert.Empty(t, resp.ResourceSchemas)
		})
	}
}

func TestProviderSchema(t *testing.T) {
	attributes := providerSchema(t).Attributes
	tests := []struct {
		name      string
		sensitive bool
		envs      []string
	}{
		{name: "server_url", envs: []string{"STACKORDER_SERVER_URL"}},
		{name: "api_key", sensitive: true, envs: []string{"STACKORDER_API_KEY"}},
		{name: "repository", envs: []string{"STACKORDER_REPOSITORY", "GITHUB_REPOSITORY"}},
		{name: "oidc_audience", envs: []string{"STACKORDER_OIDC_AUDIENCE"}},
	}
	require.Len(t, attributes, len(tests))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Contains(t, attributes, tt.name)
			a, ok := attributes[tt.name].(schema.StringAttribute)
			require.True(t, ok, "%s is %T", tt.name, attributes[tt.name])
			assert.True(t, a.IsOptional())
			assert.False(t, a.IsRequired())
			assert.False(t, a.IsComputed())
			assert.Equal(t, tt.sensitive, a.IsSensitive())
			for _, env := range tt.envs {
				assert.Contains(t, a.MarkdownDescription, "`"+env+"`")
			}
		})
	}
}

func TestConfigureServerURL(t *testing.T) {
	srv := newFakeStackorder(t)
	tests := []struct {
		name          string
		config        map[string]tftypes.Value
		env           map[string]string
		wantPrefix    string
		wantAttribute string
		wantSummary   string
		wantDetail    string
	}{
		{
			name:       "from the attribute",
			config:     map[string]tftypes.Value{"server_url": known(srv.url + "/attribute")},
			wantPrefix: "/attribute",
		},
		{
			name:       "from STACKORDER_SERVER_URL",
			env:        map[string]string{"STACKORDER_SERVER_URL": srv.url + "/environment"},
			wantPrefix: "/environment",
		},
		{
			name:       "attribute over the environment",
			config:     map[string]tftypes.Value{"server_url": known(srv.url + "/attribute")},
			env:        map[string]string{"STACKORDER_SERVER_URL": srv.url + "/environment"},
			wantPrefix: "/attribute",
		},
		{
			name:       "blank attribute falls back to the environment",
			config:     map[string]tftypes.Value{"server_url": known(" ")},
			env:        map[string]string{"STACKORDER_SERVER_URL": srv.url + "/environment"},
			wantPrefix: "/environment",
		},
		{
			name:       "surrounding whitespace trimmed",
			config:     map[string]tftypes.Value{"server_url": known(" " + srv.url + "/attribute/\n")},
			wantPrefix: "/attribute",
		},
		{
			name:          "missing",
			wantAttribute: "server_url",
			wantSummary:   "Missing Stackorder server URL",
			wantDetail:    "STACKORDER_SERVER_URL",
		},
		{
			name:          "not an http URL",
			config:        map[string]tftypes.Value{"server_url": known("ftp://stackorder.acme.example")},
			wantAttribute: "server_url",
			wantSummary:   "Invalid Stackorder server URL",
			wantDetail:    "The URL in server_url is not usable: the Stackorder server URL is not an http or https URL with a host.",
		},
		{
			name:          "user info",
			config:        map[string]tftypes.Value{"server_url": known("https://admin:" + testURLPassword + "@stackorder.acme.example")},
			wantAttribute: "server_url",
			wantSummary:   "Invalid Stackorder server URL",
			wantDetail:    "The URL in server_url is not usable: the Stackorder server URL carries user info, a query or a fragment.",
		},
		{
			name:          "user info without a scheme",
			config:        map[string]tftypes.Value{"server_url": known("admin:" + testURLPassword + "@stackorder.acme.example")},
			wantAttribute: "server_url",
			wantSummary:   "Invalid Stackorder server URL",
			wantDetail:    "The URL in server_url is not usable: the Stackorder server URL is not an http or https URL with a host.",
		},
		{
			name:          "key in the query",
			config:        map[string]tftypes.Value{"server_url": known("https://stackorder.acme.example/?key=" + testAPIKey)},
			wantAttribute: "server_url",
			wantSummary:   "Invalid Stackorder server URL",
			wantDetail:    "The URL in server_url is not usable: the Stackorder server URL carries user info, a query or a fragment.",
		},
		{
			name:        "invalid STACKORDER_SERVER_URL",
			env:         map[string]string{"STACKORDER_SERVER_URL": "stackorder.acme.example"},
			wantSummary: "Invalid Stackorder server URL",
			wantDetail:  "The URL in STACKORDER_SERVER_URL is not usable: the Stackorder server URL is not an http or https URL with a host.",
		},
		{
			name:        "password read as a port in STACKORDER_SERVER_URL",
			env:         map[string]string{"STACKORDER_SERVER_URL": "https://admin:" + testURLPassword},
			wantSummary: "Invalid Stackorder server URL",
			wantDetail:  "The URL in STACKORDER_SERVER_URL is not usable: the Stackorder server URL does not parse as a URL.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, tt.env)
			t.Setenv("STACKORDER_API_KEY", testAPIKey)

			resp := configure(t, tt.config)
			if tt.wantSummary != "" {
				d := requireError(t, resp, tt.wantAttribute, tt.wantSummary)
				assert.Contains(t, d.Detail(), tt.wantDetail)
				return
			}
			got := srv.read(t, configured(t, resp))
			assert.Equal(t, tt.wantPrefix+testOutputsPath, got.path)
			assert.Equal(t, "Bearer "+testAPIKey, got.auth)
			assert.Equal(t, "terraform-provider-stackorder/test", got.userAgent)
		})
	}
}

func TestConfigureCredentials(t *testing.T) {
	srv := newFakeStackorder(t)
	job := map[string]string{"STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader}
	tests := []struct {
		name       string
		actions    bool
		config     map[string]tftypes.Value
		env        map[string]string
		wantAuth   string
		wantRunID  string
		wantReader string
	}{
		{
			name:     "API key from the attribute",
			config:   map[string]tftypes.Value{"api_key": known(testAPIKey)},
			wantAuth: "Bearer " + testAPIKey,
		},
		{
			name:     "API key from STACKORDER_API_KEY",
			env:      map[string]string{"STACKORDER_API_KEY": testAPIKey},
			wantAuth: "Bearer " + testAPIKey,
		},
		{
			name:     "attribute key over the environment",
			config:   map[string]tftypes.Value{"api_key": known("sk_acme_attribute")},
			env:      map[string]string{"STACKORDER_API_KEY": testAPIKey},
			wantAuth: "Bearer sk_acme_attribute",
		},
		{
			name:       "API key in a Stackorder job outside GitHub Actions",
			env:        map[string]string{"STACKORDER_API_KEY": testAPIKey, "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAuth:   "Bearer " + testAPIKey,
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "ID token in a Stackorder job",
			actions:    true,
			env:        job,
			wantAuth:   "Bearer id-token-for:" + srv.url,
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "ID token audience from oidc_audience",
			actions:    true,
			config:     map[string]tftypes.Value{"oidc_audience": known("stackorder-acme")},
			env:        map[string]string{"STACKORDER_OIDC_AUDIENCE": "stackorder-environment", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAuth:   "Bearer id-token-for:stackorder-acme",
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "ID token audience from STACKORDER_OIDC_AUDIENCE",
			actions:    true,
			env:        map[string]string{"STACKORDER_OIDC_AUDIENCE": "stackorder-environment", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAuth:   "Bearer id-token-for:stackorder-environment",
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "ID token audience from STACKORDER_SERVER_URL as set",
			actions:    true,
			config:     map[string]tftypes.Value{"server_url": tftypes.NewValue(tftypes.String, nil)},
			env:        map[string]string{"STACKORDER_SERVER_URL": srv.url + "/", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAuth:   "Bearer id-token-for:" + srv.url + "/",
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "ID token over a configured API key",
			actions:    true,
			config:     map[string]tftypes.Value{"api_key": known("sk_acme_attribute")},
			env:        map[string]string{"STACKORDER_API_KEY": testAPIKey, "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAuth:   "Bearer id-token-for:" + srv.url,
			wantRunID:  testRunID,
			wantReader: testReader,
		},
		{
			name:       "run and stack trimmed",
			actions:    true,
			env:        map[string]string{"STACKORDER_RUN_ID": " " + testRunID + "\n", "STACKORDER_STACK": "\t" + testReader + " "},
			wantAuth:   "Bearer id-token-for:" + srv.url,
			wantRunID:  testRunID,
			wantReader: testReader,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, tt.env)
			if tt.actions {
				fakeActions(t)
			}
			config := map[string]tftypes.Value{"server_url": known(srv.url)}
			for name, v := range tt.config {
				config[name] = v
			}

			data := configured(t, configure(t, config))
			assert.Equal(t, tt.wantRunID, data.RunID)
			assert.Equal(t, tt.wantReader, data.Reader)

			got := srv.read(t, data)
			assert.Equal(t, testOutputsPath, got.path)
			assert.Equal(t, tt.wantAuth, got.auth)
			wantQuery := url.Values{}
			if tt.wantRunID != "" {
				wantQuery.Set("run", tt.wantRunID)
				wantQuery.Set("reader", tt.wantReader)
			}
			assert.Equal(t, wantQuery, got.query)
		})
	}
}

func TestConfigureCredentialErrors(t *testing.T) {
	tests := []struct {
		name          string
		actions       bool
		config        map[string]tftypes.Value
		env           map[string]string
		wantAttribute string
		wantSummary   string
		wantDetail    []string
	}{
		{
			name:          "no API key outside GitHub Actions",
			env:           map[string]string{"STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			wantAttribute: "api_key",
			wantSummary:   "Missing Stackorder credentials",
			wantDetail:    []string{"api_key", "STACKORDER_API_KEY", "id-token: write"},
		},
		{
			name:        "ID token without STACKORDER_RUN_ID",
			actions:     true,
			env:         map[string]string{"STACKORDER_STACK": testReader},
			wantSummary: "Not inside a Stackorder job",
			wantDetail:  []string{"STACKORDER_RUN_ID is not set.", "plan, apply or drift job", "api_key", "STACKORDER_API_KEY"},
		},
		{
			name:        "ID token without STACKORDER_STACK",
			actions:     true,
			env:         map[string]string{"STACKORDER_RUN_ID": testRunID},
			wantSummary: "Not inside a Stackorder job",
			wantDetail:  []string{"STACKORDER_STACK is not set.", "plan, apply or drift job", "api_key"},
		},
		{
			name:        "ID token outside any Stackorder job",
			actions:     true,
			wantSummary: "Not inside a Stackorder job",
			wantDetail:  []string{"STACKORDER_RUN_ID and STACKORDER_STACK are not set.", "plan, apply or drift job", "api_key"},
		},
		{
			name:        "ID token outside a Stackorder job despite an API key",
			actions:     true,
			config:      map[string]tftypes.Value{"api_key": known(testAPIKey)},
			env:         map[string]string{"STACKORDER_API_KEY": testAPIKey, "STACKORDER_RUN_ID": " "},
			wantSummary: "Not inside a Stackorder job",
			wantDetail:  []string{"STACKORDER_RUN_ID and STACKORDER_STACK are not set.", "take precedence over an API key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, tt.env)
			if tt.actions {
				fakeActions(t)
			}
			config := map[string]tftypes.Value{"server_url": known("https://stackorder.acme.example")}
			for name, v := range tt.config {
				config[name] = v
			}

			d := requireError(t, configure(t, config), tt.wantAttribute, tt.wantSummary)
			for _, want := range tt.wantDetail {
				assert.Contains(t, d.Detail(), want)
			}
		})
	}
}

func TestConfigureRepository(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]tftypes.Value
		env    map[string]string
		want   string
	}{
		{
			name:   "attribute over both variables",
			config: map[string]tftypes.Value{"repository": known("acme/platform")},
			env:    map[string]string{"STACKORDER_REPOSITORY": "acme/network", "GITHUB_REPOSITORY": "acme/infra"},
			want:   "acme/platform",
		},
		{
			name: "STACKORDER_REPOSITORY over GITHUB_REPOSITORY",
			env:  map[string]string{"STACKORDER_REPOSITORY": "acme/network", "GITHUB_REPOSITORY": "acme/infra"},
			want: "acme/network",
		},
		{
			name: "GITHUB_REPOSITORY last",
			env:  map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			want: "acme/infra",
		},
		{
			name:   "blank attribute falls back to the environment",
			config: map[string]tftypes.Value{"repository": known("")},
			env:    map[string]string{"STACKORDER_REPOSITORY": " ", "GITHUB_REPOSITORY": "acme/infra"},
			want:   "acme/infra",
		},
		{
			name:   "surrounding whitespace trimmed",
			config: map[string]tftypes.Value{"repository": known(" acme/platform\n")},
			want:   "acme/platform",
		},
		{
			name: "unnamed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, tt.env)
			t.Setenv("STACKORDER_SERVER_URL", "https://stackorder.acme.example")
			t.Setenv("STACKORDER_API_KEY", testAPIKey)

			data := configured(t, configure(t, tt.config))
			assert.Equal(t, tt.want, data.Repository)
		})
	}
}

func TestConfigureRefusesUnknownValues(t *testing.T) {
	tests := []struct {
		name    string
		unknown []string
		want    map[string]string
	}{
		{name: "server_url", unknown: []string{"server_url"}, want: map[string]string{"server_url": "STACKORDER_SERVER_URL"}},
		{name: "api_key", unknown: []string{"api_key"}, want: map[string]string{"api_key": "STACKORDER_API_KEY"}},
		{name: "repository", unknown: []string{"repository"}, want: map[string]string{"repository": "STACKORDER_REPOSITORY"}},
		{name: "oidc_audience", unknown: []string{"oidc_audience"}, want: map[string]string{"oidc_audience": "STACKORDER_OIDC_AUDIENCE"}},
		{
			name:    "every attribute",
			unknown: []string{"server_url", "api_key", "repository", "oidc_audience"},
			want: map[string]string{
				"server_url":    "STACKORDER_SERVER_URL",
				"api_key":       "STACKORDER_API_KEY",
				"repository":    "STACKORDER_REPOSITORY",
				"oidc_audience": "STACKORDER_OIDC_AUDIENCE",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, map[string]string{
				"STACKORDER_SERVER_URL": "https://stackorder.acme.example",
				"STACKORDER_API_KEY":    testAPIKey,
			})
			config := map[string]tftypes.Value{}
			for _, name := range tt.unknown {
				config[name] = unknown()
			}

			resp := configure(t, config)
			assert.Nil(t, resp.DataSourceData)
			assert.Nil(t, resp.EphemeralResourceData)
			got := map[string]string{}
			for _, d := range resp.Diagnostics {
				assert.Equal(t, diag.SeverityError, d.Severity())
				assert.Contains(t, d.Summary(), "Unknown Stackorder")
				attribute := diagnosticPath(d)
				assert.Contains(t, d.Detail(), attribute+" is unknown")
				assert.Contains(t, d.Detail(), "known at plan time")
				if env := tt.want[attribute]; env != "" {
					assert.Contains(t, d.Detail(), env)
				}
				got[attribute] = tt.want[attribute]
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestConfigureProviderOverProtocol6(t *testing.T) {
	configType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"server_url":    tftypes.String,
		"api_key":       tftypes.String,
		"repository":    tftypes.String,
		"oidc_audience": tftypes.String,
	}}
	tests := []struct {
		name          string
		serverURL     tftypes.Value
		wantAttribute string
		wantSummary   string
	}{
		{name: "configured", serverURL: known("https://stackorder.acme.example")},
		{name: "missing server URL", serverURL: tftypes.NewValue(tftypes.String, nil), wantAttribute: "server_url", wantSummary: "Missing Stackorder server URL"},
		{name: "unknown server URL", serverURL: unknown(), wantAttribute: "server_url", wantSummary: "Unknown Stackorder server URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanEnvironment(t, nil)
			server, err := providerserver.NewProtocol6WithError(New("test")())()
			require.NoError(t, err)
			config, err := tfprotov6.NewDynamicValue(configType, tftypes.NewValue(configType, map[string]tftypes.Value{
				"server_url":    tt.serverURL,
				"api_key":       known(testAPIKey),
				"repository":    known("acme/platform"),
				"oidc_audience": tftypes.NewValue(tftypes.String, nil),
			}))
			require.NoError(t, err)

			resp, err := server.ConfigureProvider(t.Context(), &tfprotov6.ConfigureProviderRequest{TerraformVersion: "1.16.5", Config: &config})
			require.NoError(t, err)
			if tt.wantSummary == "" {
				assert.Empty(t, resp.Diagnostics)
				return
			}
			require.Len(t, resp.Diagnostics, 1)
			d := resp.Diagnostics[0]
			assert.Equal(t, tfprotov6.DiagnosticSeverityError, d.Severity)
			assert.Equal(t, tt.wantSummary, d.Summary)
			assert.True(t, d.Attribute.Equal(tftypes.NewAttributePath().WithAttributeName(tt.wantAttribute)), "attribute %s", d.Attribute)
			assert.NotContains(t, d.Detail, testAPIKey)
		})
	}
}
