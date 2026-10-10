package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/stackorder/stackorder/api/v1"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
)

const (
	vpcStack         = "stacks/prod/vpc"
	vpcID            = "acme/infra//stacks/prod/vpc"
	vpcOutputsPath   = "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fvpc/outputs"
	clusterStack     = "acme/platform//clusters/prod"
	clusterPath      = "/v1/repos/acme/platform/stacks/clusters%2Fprod/outputs"
	pskCanary        = "psk-canary-7c1d9e"
	testServerHost   = "stackorder.acme.example"
	outputsAddress   = "data.stackorder_outputs.vpc"
	outputsTypeName  = "stackorder_outputs"
	withheldSummary  = "Sensitive outputs withheld"
	notSharedSummary = "Stack outputs not shared"
)

type served struct {
	status int
	body   []byte
	echo   bool
}

type outputsServer struct {
	url     string
	mu      sync.Mutex
	answers map[string]served
	got     []request
}

func newOutputsServer(t *testing.T, answers map[string]served) *outputsServer {
	t.Helper()
	s := &outputsServer{answers: maps.Clone(answers)}
	if s.answers == nil {
		s.answers = map[string]served{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.got = append(s.got, request{
			path:      r.URL.EscapedPath(),
			query:     r.URL.Query(),
			auth:      r.Header.Get("Authorization"),
			userAgent: r.Header.Get("User-Agent"),
		})
		a, ok := s.answers[r.URL.EscapedPath()]
		s.mu.Unlock()
		if !ok {
			a = served{status: http.StatusNotFound, body: refusalBody(codeNotFound, "no such stack")}
		}
		if a.echo {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(a.status)
			_, _ = fmt.Fprintf(w, "<html>bad gateway for %s auth=%s</html>", r.URL.RequestURI(), r.Header.Get("Authorization"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = w.Write(a.body)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *outputsServer) serve(path string, a served) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[path] = a
}

func (s *outputsServer) requests() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func okWith(body []byte) served { return served{status: http.StatusOK, body: body} }

func golden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "client", "testdata", name))
	require.NoError(t, err)
	return raw
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func sharingSensitive(t *testing.T) []byte {
	t.Helper()
	var out v1.StackOutputs
	require.NoError(t, json.Unmarshal(golden(t, "stack_outputs.json"), &out))
	out.Outputs["vpn_preshared_key"] = v1.StackOutput{
		Value:     json.RawMessage(strconv.Quote(pskCanary)),
		Type:      json.RawMessage(`"string"`),
		Sensitive: true,
	}
	out.Withheld = nil
	return encode(t, out)
}

func refusalBody(code, message string) []byte {
	raw, _ := json.Marshal(v1.Error{Code: code, Message: message})
	return raw
}

func str(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }

func emptyObject() tftypes.Value {
	return tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{}}, map[string]tftypes.Value{})
}

func vpcValues(extra map[string]tftypes.Value) tftypes.Value {
	endpoints := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"dynamodb": tftypes.String, "s3": tftypes.String}}
	subnets := tftypes.List{ElementType: tftypes.String}
	tags := tftypes.Map{ElementType: tftypes.String}
	attrs := map[string]tftypes.Value{
		"endpoints": tftypes.NewValue(endpoints, map[string]tftypes.Value{
			"dynamodb": str("vpce-0d4e5f6a7b8c9d0e1"),
			"s3":       str("vpce-0a1b2c3d4e5f6a7b8"),
		}),
		"nat_gateway_count":  tftypes.NewValue(tftypes.Number, big.NewFloat(3)),
		"private_subnet_ids": tftypes.NewValue(subnets, []tftypes.Value{str("subnet-0a1b2c3d4e5f6a7b8"), str("subnet-0b2c3d4e5f6a7b8c9")}),
		"tags":               tftypes.NewValue(tags, map[string]tftypes.Value{"environment": str("prod"), "team": str("platform")}),
		"vpc_id":             str("vpc-0a1b2c3d4e5f67890"),
	}
	maps.Copy(attrs, extra)
	attrTypes := make(map[string]tftypes.Type, len(attrs))
	for name, v := range attrs {
		attrTypes[name] = v.Type()
	}
	return tftypes.NewValue(tftypes.Object{AttributeTypes: attrTypes}, attrs)
}

func nullObject(block *tfprotov6.SchemaBlock, set map[string]tftypes.Value) tftypes.Value {
	attrs := make(map[string]tftypes.Value, len(block.Attributes))
	for _, a := range block.Attributes {
		attrs[a.Name] = tftypes.NewValue(a.ValueType(), nil)
	}
	maps.Copy(attrs, set)
	return tftypes.NewValue(block.ValueType(), attrs)
}

func configuredServer(t *testing.T) (tfprotov6.ProviderServer, *tfprotov6.GetProviderSchemaResponse) {
	t.Helper()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	require.NoError(t, err)
	schemas, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)

	providerConfig, err := tfprotov6.NewDynamicValue(schemas.Provider.ValueType(), nullObject(schemas.Provider.Block, nil))
	require.NoError(t, err)
	configured, err := server.ConfigureProvider(t.Context(), &tfprotov6.ConfigureProviderRequest{TerraformVersion: "1.14.4", Config: &providerConfig})
	require.NoError(t, err)
	require.Empty(t, configured.Diagnostics)
	return server, schemas
}

func assertNoSecretsServed(t *testing.T, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		for _, secret := range []string{testAPIKey, testRequestToken, pskCanary} {
			assert.NotContains(t, d.Summary+" "+d.Detail, secret)
		}
	}
}

func objectAttributes(t *testing.T, raw *tfprotov6.DynamicValue, typ tftypes.Type) map[string]tftypes.Value {
	t.Helper()
	if raw == nil {
		return nil
	}
	v, err := raw.Unmarshal(typ)
	require.NoError(t, err)
	if v.IsNull() {
		return nil
	}
	var attrs map[string]tftypes.Value
	require.NoError(t, v.As(&attrs))
	return attrs
}

func readThroughServer(t *testing.T, stack string) (map[string]tftypes.Value, []*tfprotov6.Diagnostic) {
	t.Helper()
	server, schemas := configuredServer(t)
	ds := schemas.DataSourceSchemas[outputsTypeName]
	require.NotNil(t, ds)
	config, err := tfprotov6.NewDynamicValue(ds.ValueType(), nullObject(ds.Block, map[string]tftypes.Value{"stack": str(stack)}))
	require.NoError(t, err)
	read, err := server.ReadDataSource(t.Context(), &tfprotov6.ReadDataSourceRequest{TypeName: outputsTypeName, Config: &config})
	require.NoError(t, err)
	assertNoSecretsServed(t, read.Diagnostics)
	return objectAttributes(t, read.State, ds.ValueType()), read.Diagnostics
}

func serverEnvironment(t *testing.T, srv *outputsServer, env map[string]string) {
	t.Helper()
	all := map[string]string{"STACKORDER_SERVER_URL": srv.url, "STACKORDER_API_KEY": testAPIKey}
	maps.Copy(all, env)
	cleanEnvironment(t, all)
}

func TestOutputsDataSourceSchema(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	require.NoError(t, err)
	schemas, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	require.NoError(t, err)
	ds := schemas.DataSourceSchemas[outputsTypeName]
	require.NotNil(t, ds)

	type flags struct{ required, computed, sensitive bool }
	got := map[string]flags{}
	types := map[string]tftypes.Type{}
	for _, a := range ds.Block.Attributes {
		got[a.Name] = flags{required: a.Required, computed: a.Computed, sensitive: a.Sensitive}
		types[a.Name] = a.Type
	}
	assert.Equal(t, map[string]flags{
		"stack":               {required: true},
		"id":                  {computed: true},
		"values":              {computed: true, sensitive: true},
		"nonsensitive_values": {computed: true},
	}, got)
	assert.Equal(t, map[string]tftypes.Type{
		"stack":               tftypes.String,
		"id":                  tftypes.String,
		"values":              tftypes.DynamicPseudoType,
		"nonsensitive_values": tftypes.DynamicPseudoType,
	}, types)
	for _, want := range []string{"outputs.readers", "outputs.sensitive: share", "the data source warns", "default branch", "plan file", `ephemeral "stackorder_outputs"`, "Terraform 1.10", "OpenTofu 1.11"} {
		assert.Contains(t, ds.Block.Description, want)
	}
}

type outputsReadCase struct {
	name             string
	env              map[string]string
	stack            string
	path             string
	body             []byte
	wantQuery        url.Values
	wantID           string
	wantValues       tftypes.Value
	wantNonsensitive tftypes.Value
	wantWithheld     []string
}

func outputsReadCases(t *testing.T) []outputsReadCase {
	t.Helper()
	return []outputsReadCase{
		{
			name:             "stack of GITHUB_REPOSITORY",
			env:              map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:            vpcStack,
			path:             vpcOutputsPath,
			body:             golden(t, "stack_outputs.json"),
			wantQuery:        url.Values{},
			wantID:           vpcID,
			wantValues:       vpcValues(nil),
			wantNonsensitive: vpcValues(nil),
			wantWithheld:     []string{`"vpn_preshared_key"`},
		},
		{
			name:             "run and reader of a Stackorder job",
			env:              map[string]string{"STACKORDER_REPOSITORY": "acme/infra", "GITHUB_REPOSITORY": "acme/other", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			stack:            vpcStack,
			path:             vpcOutputsPath,
			body:             golden(t, "stack_outputs.json"),
			wantQuery:        url.Values{"run": {testRunID}, "reader": {testReader}},
			wantID:           vpcID,
			wantValues:       vpcValues(nil),
			wantNonsensitive: vpcValues(nil),
			wantWithheld:     []string{`"vpn_preshared_key"`},
		},
		{
			name:             "shared sensitive output",
			env:              map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:            vpcStack,
			path:             vpcOutputsPath,
			body:             sharingSensitive(t),
			wantQuery:        url.Values{},
			wantID:           vpcID,
			wantValues:       vpcValues(map[string]tftypes.Value{"vpn_preshared_key": str(pskCanary)}),
			wantNonsensitive: vpcValues(nil),
		},
		{
			name:             "stack of another repository",
			env:              map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:            clusterStack,
			path:             clusterPath,
			body:             golden(t, "stack_outputs_empty.json"),
			wantQuery:        url.Values{},
			wantID:           clusterStack,
			wantValues:       emptyObject(),
			wantNonsensitive: emptyObject(),
		},
		{
			name:             "instance of a stack",
			env:              map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:            "stacks/prod/apps:blue",
			path:             "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fapps:blue/outputs",
			body:             golden(t, "stack_outputs_empty.json"),
			wantQuery:        url.Values{},
			wantID:           "acme/infra//stacks/prod/apps:blue",
			wantValues:       emptyObject(),
			wantNonsensitive: emptyObject(),
		},
		{
			name:  "several outputs withheld",
			env:   map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack: vpcStack,
			path:  vpcOutputsPath,
			body: encode(t, v1.StackOutputs{
				Repo: "acme/infra", Stack: vpcStack,
				Outputs:  map[string]v1.StackOutput{},
				Withheld: []string{"vpn_preshared_key", "db_password"},
			}),
			wantQuery:        url.Values{},
			wantID:           vpcID,
			wantValues:       emptyObject(),
			wantNonsensitive: emptyObject(),
			wantWithheld:     []string{`"db_password", "vpn_preshared_key"`},
		},
	}
}

func (tt outputsReadCase) assertRead(t *testing.T, srv *outputsServer, attrs map[string]tftypes.Value, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	got := srv.requests()
	require.Len(t, got, 1)
	assert.Equal(t, tt.path, got[0].path)
	assert.Equal(t, tt.wantQuery, got[0].query)
	assert.Equal(t, "Bearer "+testAPIKey, got[0].auth)
	require.NotNil(t, attrs, "%v", diags)
	assert.True(t, attrs["stack"].Equal(str(tt.stack)), "stack %s", attrs["stack"])
	assert.True(t, attrs["values"].Type().Equal(tt.wantValues.Type()), "values type %s", attrs["values"].Type())
	assert.True(t, attrs["values"].Equal(tt.wantValues), "values %s", attrs["values"])
	assert.True(t, attrs["nonsensitive_values"].Type().Equal(tt.wantNonsensitive.Type()), "nonsensitive_values type %s", attrs["nonsensitive_values"].Type())
	assert.True(t, attrs["nonsensitive_values"].Equal(tt.wantNonsensitive), "nonsensitive_values %s", attrs["nonsensitive_values"])
	if tt.wantWithheld == nil {
		assert.Empty(t, diags)
		return
	}
	require.Len(t, diags, 1)
	assert.Equal(t, tfprotov6.DiagnosticSeverityWarning, diags[0].Severity)
	assert.Equal(t, withheldSummary, diags[0].Summary)
	for _, want := range append(tt.wantWithheld, tt.wantID, "outputs.sensitive: share", "default branch", `data "stackorder_outputs"`, `ephemeral "stackorder_outputs"`) {
		assert.Contains(t, diags[0].Detail, want)
	}
}

func TestOutputsDataSourceRead(t *testing.T) {
	for _, tt := range outputsReadCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOutputsServer(t, map[string]served{tt.path: okWith(tt.body)})
			serverEnvironment(t, srv, tt.env)

			attrs, diags := readThroughServer(t, tt.stack)

			tt.assertRead(t, srv, attrs, diags)
			assert.True(t, attrs["id"].Equal(str(tt.wantID)), "id %s", attrs["id"])
		})
	}
}

type outputsReadErrorCase struct {
	name          string
	env           map[string]string
	stack         string
	answers       map[string]served
	wantRequests  int
	wantSummary   string
	wantAttribute bool
	wantDetail    []string
	notDetail     []string
}

func outputsReadErrorCases(t *testing.T) []outputsReadErrorCase {
	t.Helper()
	malformed := encode(t, v1.StackOutputs{Repo: "acme/infra", Stack: vpcStack, Outputs: map[string]v1.StackOutput{
		"vpn_preshared_key": {Value: json.RawMessage(strconv.Quote(pskCanary)), Type: json.RawMessage(`"number"`), Sensitive: true},
	}})
	return []outputsReadErrorCase{
		{
			name:          "no repository for a stack of the same repository",
			stack:         vpcStack,
			wantSummary:   "Missing Stackorder repository",
			wantAttribute: true,
			wantDetail:    []string{"repository", "STACKORDER_REPOSITORY", "GITHUB_REPOSITORY", "owner/repo//stacks/prod/vpc"},
		},
		{
			name:          "empty stack",
			env:           map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:         "",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{"path, path:instance or owner/repo//key"},
		},
		{
			name:          "parent directory as the stack",
			env:           map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:         "..",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{`".." is not the key of a stack`, "path, path:instance or owner/repo//key"},
		},
		{
			name:          "repository setting not owner/repo",
			env:           map[string]string{"GITHUB_REPOSITORY": "acme"},
			stack:         vpcStack,
			wantSummary:   "Invalid Stackorder repository",
			wantAttribute: true,
			wantDetail:    []string{`the first of them set holds "acme", which is not owner/repo`, "repository", "STACKORDER_REPOSITORY", "GITHUB_REPOSITORY", "owner/repo//stacks/prod/vpc"},
		},
		{
			name:          "repository part with a space",
			env:           map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:         "acme/in fra//stacks/prod/vpc",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{`"acme/in fra//stacks/prod/vpc"`, "owner/repo//key"},
		},
		{
			name:          "repository part without an owner",
			stack:         "infra//stacks/prod/vpc",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{`"infra//stacks/prod/vpc"`, "owner/repo//key"},
		},
		{
			name:          "repository part with a path",
			stack:         "acme/infra/stacks//prod/vpc",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{"owner/repo//key"},
		},
		{
			name:          "no key after the repository",
			stack:         "acme/platform//",
			wantSummary:   "Invalid stack",
			wantAttribute: true,
			wantDetail:    []string{"owner/repo//key"},
		},
		{
			name:          "not shared with the reader",
			env:           map[string]string{"STACKORDER_REPOSITORY": "acme/infra", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			stack:         vpcStack,
			answers:       map[string]served{vpcOutputsPath: {status: http.StatusForbidden, body: refusalBody(v1.ErrCodeOutputsNotShared, "stacks/prod/vpc does not share its outputs with stacks/prod/eks")}},
			wantRequests:  1,
			wantSummary:   notSharedSummary,
			wantAttribute: true,
			wantDetail:    []string{vpcID, "      - stacks/prod/eks", "outputs.readers", "The server answered 403 outputs_not_shared: stacks/prod/vpc does not share"},
		},
		{
			name:         "proxy echoing the request",
			env:          map[string]string{"GITHUB_REPOSITORY": "acme/infra", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader},
			stack:        vpcStack,
			answers:      map[string]served{vpcOutputsPath: {status: http.StatusBadGateway, echo: true}},
			wantRequests: client.DefaultAttempts,
			wantSummary:  "Stackorder server unavailable",
			wantDetail:   []string{"127.0.0.1", "Retry later", "The server answered 502 Bad Gateway."},
			notDetail:    []string{"/v1/repos", "bad gateway for", "auth=", "Bearer", testRunID},
		},
		{
			name:          "unknown stack",
			env:           map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:         "stacks/prod/missing",
			wantRequests:  1,
			wantSummary:   "Stack not found",
			wantAttribute: true,
			wantDetail:    []string{"acme/infra//stacks/prod/missing", "The server answered 404 not_found: no such stack."},
		},
		{
			name:         "output not of its type",
			env:          map[string]string{"GITHUB_REPOSITORY": "acme/infra"},
			stack:        vpcStack,
			answers:      map[string]served{vpcOutputsPath: okWith(malformed)},
			wantRequests: 1,
			wantSummary:  "Malformed stack outputs",
			wantDetail:   []string{`output "vpn_preshared_key"`, "does not conform to its type", "127.0.0.1"},
		},
	}
}

func (tt outputsReadErrorCase) assertFailure(t *testing.T, srv *outputsServer, attrs map[string]tftypes.Value, diags []*tfprotov6.Diagnostic) {
	t.Helper()
	assert.Len(t, srv.requests(), tt.wantRequests)
	for _, name := range []string{"values", "nonsensitive_values"} {
		assert.True(t, attrs[name].IsNull(), "%s %s", name, attrs[name])
	}
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, tfprotov6.DiagnosticSeverityError, d.Severity)
	assert.Equal(t, tt.wantSummary, d.Summary)
	if tt.wantAttribute {
		assert.True(t, d.Attribute.Equal(tftypes.NewAttributePath().WithAttributeName("stack")), "attribute %s", d.Attribute)
	} else {
		assert.Nil(t, d.Attribute)
	}
	for _, want := range tt.wantDetail {
		assert.Contains(t, d.Detail, want)
	}
	for _, unwanted := range append(tt.notDetail, testAPIKey) {
		assert.NotContains(t, d.Summary+" "+d.Detail, unwanted)
	}
}

func TestOutputsDataSourceReadErrors(t *testing.T) {
	for _, tt := range outputsReadErrorCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			srv := newOutputsServer(t, tt.answers)
			serverEnvironment(t, srv, tt.env)

			attrs, diags := readThroughServer(t, tt.stack)

			tt.assertFailure(t, srv, attrs, diags)
			assert.True(t, attrs["id"].IsNull(), "id %s", attrs["id"])
		})
	}
}

func TestReadFailureDiagnostics(t *testing.T) {
	refusal := func(status int, code, message string) error {
		return fmt.Errorf("reading the outputs of %s: %w", vpcID, &client.APIError{Status: status, Code: code, Message: message})
	}
	retried := func(status int, code, message string) error {
		return fmt.Errorf("reading the outputs of %s: giving up after 3 attempts: %w", vpcID, &client.APIError{Status: status, Code: code, Message: message})
	}
	transport := func(rawURL string) error {
		return fmt.Errorf("reading the outputs of %s: giving up after 3 attempts: %w", vpcID,
			&url.Error{Op: "Get", URL: rawURL, Err: errors.New("dial tcp 192.0.2.10:443: connect: connection refused")})
	}
	serverURL := "https://" + testServerHost + "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fvpc/outputs?reader=stacks%2Fprod%2Feks&run=" + testRunID
	tokenURL := "https://pipelines.actions.githubusercontent.com/acme/idtoken?api-version=2.0&audience=https%3A%2F%2F" + testServerHost

	tests := []struct {
		name          string
		err           error
		repo          string
		idTokens      bool
		reader        string
		wantSummary   string
		wantAttribute bool
		wantDetail    []string
		notDetail     []string
		exactDetail   string
	}{
		{
			name:          v1.ErrCodeOutputsNotShared,
			err:           refusal(http.StatusForbidden, v1.ErrCodeOutputsNotShared, "not shared."),
			wantSummary:   notSharedSummary,
			wantAttribute: true,
			wantDetail: []string{
				"The stack " + vpcID + " does not share its outputs with the stack stacks/prod/eks: either outputs.readers on its default branch does not list it, or that outputs configuration does not resolve",
				"      - stacks/prod/eks\n", "The server answered 403 outputs_not_shared: not shared.",
			},
		},
		{
			name:          v1.ErrCodeOutputsNotShared + " with an outputs configuration that does not resolve",
			err:           refusal(http.StatusForbidden, v1.ErrCodeOutputsNotShared, "the outputs configuration of stacks/prod/vpc on the default branch does not resolve, so its outputs are not shared: outputs.readers: not a list"),
			wantSummary:   notSharedSummary,
			wantAttribute: true,
			wantDetail:    []string{"does not resolve, as the server's answer below says", "The server answered 403 outputs_not_shared: the outputs configuration of stacks/prod/vpc on the default branch does not resolve"},
		},
		{
			name:          v1.ErrCodeOutputsNotShared + " names the bare reader key whatever the provider's repository",
			err:           refusal(http.StatusForbidden, v1.ErrCodeOutputsNotShared, ""),
			repo:          "acme/network",
			wantSummary:   notSharedSummary,
			wantAttribute: true,
			wantDetail:    []string{"does not share its outputs with the stack stacks/prod/eks:", "      - stacks/prod/eks\n", "The server answered 403 outputs_not_shared."},
			notDetail:     []string{"//stacks/prod/eks"},
		},
		{
			name:          v1.ErrCodeOutputsNotShared + " without a reader",
			err:           refusal(http.StatusForbidden, v1.ErrCodeOutputsNotShared, ""),
			reader:        "-",
			wantSummary:   notSharedSummary,
			wantAttribute: true,
			wantDetail:    []string{"does not share its outputs with the reading stack:", "      - <key of the reading stack>\n"},
		},
		{
			name:        v1.ErrCodeOutputsDisabled,
			err:         refusal(http.StatusNotFound, v1.ErrCodeOutputsDisabled, "shared outputs are disabled"),
			wantSummary: "Shared outputs disabled",
			wantDetail:  []string{testServerHost, "STACKORDER_STATE_BUCKETS", "operator"},
		},
		{
			name:          v1.ErrCodeStateLocationUnknown,
			err:           refusal(http.StatusConflict, v1.ErrCodeStateLocationUnknown, "no apply or drift job reported the state object"),
			wantSummary:   "State location unknown",
			wantAttribute: true,
			wantDetail:    []string{vpcID, "apply or drift job", "Stackorder CLI that reports the state object", "The server answered 409 state_location_unknown"},
		},
		{
			name:          codeNotFound,
			err:           refusal(http.StatusNotFound, codeNotFound, "no such stack"),
			wantSummary:   "Stack not found",
			wantAttribute: true,
			wantDetail:    []string{vpcID, "default branch of acme/infra", "without shared outputs"},
			notDetail:     []string{"another repository"},
		},
		{
			name:          codeNotFound + " in another repository",
			err:           refusal(http.StatusNotFound, codeNotFound, "no such stack"),
			repo:          "acme/network",
			wantSummary:   "Stack not found",
			wantAttribute: true,
			wantDetail:    []string{"acme/network//stacks/prod/vpc", "same Stackorder installation", "outputs.readers"},
		},
		{
			name:          v1.ErrCodeStateNotFound,
			err:           refusal(http.StatusNotFound, v1.ErrCodeStateNotFound, "the state object does not exist"),
			wantSummary:   "State object not found",
			wantAttribute: true,
			wantDetail:    []string{vpcID, "moved its state", "operator"},
		},
		{
			name:          v1.ErrCodeStateAccessDenied,
			err:           refusal(http.StatusForbidden, v1.ErrCodeStateAccessDenied, "access denied"),
			wantSummary:   "State object access denied",
			wantAttribute: true,
			wantDetail:    []string{"s3:GetObject", "s3:ListBucket", "kms:Decrypt", "operator"},
		},
		{
			name:          v1.ErrCodeStateBucketNotAllowed,
			err:           refusal(http.StatusForbidden, v1.ErrCodeStateBucketNotAllowed, "bucket not allowed"),
			wantSummary:   "State bucket not allowed",
			wantAttribute: true,
			wantDetail:    []string{"STACKORDER_STATE_BUCKETS", "operator"},
		},
		{
			name:          v1.ErrCodeStateEncrypted,
			err:           refusal(http.StatusUnprocessableEntity, v1.ErrCodeStateEncrypted, "encrypted state"),
			wantSummary:   "State encrypted",
			wantAttribute: true,
			wantDetail:    []string{"OpenTofu client-side state encryption", "never decrypts"},
		},
		{
			name:          v1.ErrCodeStateUnsupported,
			err:           refusal(http.StatusUnprocessableEntity, v1.ErrCodeStateUnsupported, "state version 3"),
			wantSummary:   "State format unsupported",
			wantAttribute: true,
			wantDetail:    []string{"version 4", "The server answered 422 state_unsupported: state version 3."},
		},
		{
			name:          v1.ErrCodeStateTooLarge,
			err:           refusal(http.StatusUnprocessableEntity, v1.ErrCodeStateTooLarge, "outputs exceed 8 MiB"),
			wantSummary:   "State too large",
			wantAttribute: true,
			wantDetail:    []string{"size limits", "outputs exceed 8 MiB"},
		},
		{
			name:          v1.ErrCodeStateUnreadable,
			err:           refusal(http.StatusUnprocessableEntity, v1.ErrCodeStateUnreadable, "malformed state at byte 812"),
			wantSummary:   "State unreadable",
			wantAttribute: true,
			wantDetail:    []string{"could not decode", "at byte 812"},
		},
		{
			name:        "unauthorized API key",
			err:         refusal(http.StatusUnauthorized, "unauthorized", "the API key is unknown or revoked"),
			wantSummary: "Stackorder credentials rejected",
			wantDetail:  []string{testServerHost, "api_key", "STACKORDER_API_KEY", "revoked"},
			notDetail:   []string{"oidc_audience"},
		},
		{
			name:        "unauthorized ID token",
			err:         refusal(http.StatusUnauthorized, "unauthorized", "verify runner token: audience mismatch"),
			idTokens:    true,
			wantSummary: "Stackorder credentials rejected",
			wantDetail:  []string{"GitHub Actions ID token", "oidc_audience", "STACKORDER_OIDC_AUDIENCE", "audience mismatch"},
			notDetail:   []string{"STACKORDER_API_KEY"},
		},
		{
			name:        "forbidden API key",
			err:         refusal(http.StatusForbidden, "forbidden", "forbidden"),
			wantSummary: "Stackorder server refused the read",
			wantDetail:  []string{"API key", "STACKORDER_API_KEY"},
		},
		{
			name:        "forbidden ID token",
			err:         refusal(http.StatusForbidden, "forbidden", "stacks/prod/eks is not a stack of the run"),
			idTokens:    true,
			wantSummary: "Stackorder server refused the read",
			wantDetail:  []string{"STACKORDER_RUN_ID", "STACKORDER_STACK", "has not reported its result", "is not a stack of the run"},
			notDetail:   []string{"in progress"},
		},
		{
			name:        "finished run",
			err:         refusal(http.StatusConflict, codeConflict, "run 5b1e3f0a is applied; a finished run reads no stack outputs"),
			idTokens:    true,
			wantSummary: "Stackorder run finished",
			wantDetail:  []string{testServerHost, "only while the job's run is in progress", "STACKORDER_RUN_ID", "Start a new run", "The server answered 409 conflict: run 5b1e3f0a is applied"},
		},
		{
			name:        "superseded run",
			err:         refusal(http.StatusConflict, codeSuperseded, "run 5b1e3f0a was superseded by a newer commit"),
			idTokens:    true,
			wantSummary: "Stackorder run superseded",
			wantDetail:  []string{testServerHost, "STACKORDER_RUN_ID", "newer run", "The server answered 409 superseded"},
		},
		{
			name:        "internal error",
			err:         retried(http.StatusInternalServerError, "internal", "internal error"),
			wantSummary: "Stackorder server unavailable",
			wantDetail:  []string{testServerHost, "Retry later", "The server answered 500 internal: internal error."},
		},
		{
			name:        "unavailable from a proxy",
			err:         retried(http.StatusServiceUnavailable, "", "Service Unavailable"),
			wantSummary: "Stackorder server unavailable",
			wantDetail:  []string{"Retry later", "The server answered 503 Service Unavailable."},
		},
		{
			name:        "proxy page echoing the request",
			err:         retried(http.StatusBadGateway, "", "<html>bad gateway for /v1/repos/acme/infra/stacks/stacks%2Fprod%2Fvpc/outputs?run="+testRunID+" auth=Bearer "+testAPIKey+"</html>"),
			wantSummary: "Stackorder server unavailable",
			wantDetail:  []string{testServerHost, "The server answered 502 Bad Gateway."},
			notDetail:   []string{"/v1/repos", "<html>", "Bearer", testRunID},
		},
		{
			name:        "forbidden by a proxy",
			err:         refusal(http.StatusForbidden, "", "<html><body>Forbidden for "+testRequestToken+"</body></html>"),
			wantSummary: "Stackorder server refused the read",
			wantDetail:  []string{"The server answered 403 Forbidden."},
			notDetail:   []string{"<html>"},
		},
		{
			name:        "unknown status from a proxy",
			err:         refusal(599, "", "<html>upstream</html>"),
			wantSummary: "Stackorder server unavailable",
			wantDetail:  []string{"The server answered 599."},
			notDetail:   []string{"upstream"},
		},
		{
			name:        "rate limited",
			err:         retried(http.StatusTooManyRequests, "", "Too Many Requests"),
			wantSummary: "Stackorder server unavailable",
			wantDetail:  []string{"Retry later"},
		},
		{
			name:        "invalid request",
			err:         refusal(http.StatusBadRequest, "invalid", "run is not a UUID"),
			wantSummary: "Invalid read of stack outputs",
			wantDetail:  []string{testServerHost, vpcID, "STACKORDER_RUN_ID", "STACKORDER_STACK", "The server answered 400 invalid: run is not a UUID."},
		},
		{
			name:        "unexpected coded answer",
			err:         refusal(http.StatusGone, "gone", "the route moved"),
			wantSummary: "Unexpected answer from the Stackorder server",
			wantDetail:  []string{testServerHost, vpcID, "The server answered 410 gone: the route moved."},
		},
		{
			name:        "redirect",
			err:         refusal(http.StatusFound, "", "redirected to https://login.acme.example/sso?state=abc123, which is never followed"),
			wantSummary: "Stackorder server redirected the read",
			wantDetail:  []string{testServerHost, "never follows", "server_url", "STACKORDER_SERVER_URL", "The server answered 302 Found, redirecting to login.acme.example."},
			notDetail:   []string{"/sso", "abc123", "https://"},
		},
		{
			name:        "redirect to a relative location",
			err:         refusal(http.StatusMovedPermanently, "", "redirected to /stackorder/v1/repos, which is never followed"),
			wantSummary: "Stackorder server redirected the read",
			wantDetail:  []string{"The server answered 301 Moved Permanently."},
			notDetail:   []string{"/stackorder", "redirecting to"},
		},
		{
			name:        "server unreachable",
			err:         transport(serverURL),
			wantSummary: "Stackorder server unreachable",
			wantDetail:  []string{"Stackorder server at " + testServerHost + ": dial tcp", "connection refused", "server_url", "STACKORDER_SERVER_URL", "retry later"},
			notDetail:   []string{"/v1/repos", "reader=", testRunID, "https://"},
		},
		{
			name:        "ID token endpoint unreachable",
			err:         transport(tokenURL),
			idTokens:    true,
			wantSummary: "GitHub Actions ID token unavailable",
			wantDetail:  []string{"from pipelines.actions.githubusercontent.com: dial tcp", "Retry later"},
			notDetail:   []string{"idtoken", "audience=", "https://"},
		},
		{
			name:        "undecodable answer",
			err:         fmt.Errorf("reading the outputs of %s: decoding the response: unexpected end of JSON input", vpcID),
			wantSummary: "Cannot read stack outputs",
			exactDetail: "Reading the outputs of " + vpcID + ": decoding the response: unexpected end of JSON input.",
		},
		{
			name:        "ID token endpoint refusal",
			err:         fmt.Errorf("reading the outputs of %s: obtaining a token: %w", vpcID, errors.New(`the GitHub Actions ID token endpoint answered 403: {"message":"Can't issue ID_TOKEN for audience"}`)),
			idTokens:    true,
			wantSummary: "Cannot read stack outputs",
			exactDetail: "Reading the outputs of " + vpcID + `: obtaining a token: the GitHub Actions ID token endpoint answered 403: {"message":"Can't issue ID_TOKEN for audience"}.`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := &providerData{ServerHost: testServerHost, IDTokens: tt.idTokens, Repository: "acme/infra", RunID: testRunID, Reader: testReader}
			switch tt.reader {
			case "-":
				data.Reader = ""
			case "":
			default:
				data.Reader = tt.reader
			}
			repo := tt.repo
			if repo == "" {
				repo = "acme/infra"
			}

			d := readFailure(tt.err, data, repo, vpcStack)

			assert.Equal(t, diag.SeverityError, d.Severity())
			assert.Equal(t, tt.wantSummary, d.Summary())
			if tt.wantAttribute {
				assert.Equal(t, "stack", diagnosticPath(d))
			} else {
				assert.Empty(t, diagnosticPath(d))
			}
			if tt.exactDetail != "" {
				assert.Equal(t, tt.exactDetail, d.Detail())
			}
			for _, want := range tt.wantDetail {
				assert.Contains(t, d.Detail(), want)
			}
			for _, unwanted := range append(tt.notDetail, testAPIKey, testRequestToken) {
				assert.NotContains(t, d.Detail(), unwanted)
			}
		})
	}
}

func testAccProtoV6ProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"stackorder": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func vpcCheck(extra map[string]knownvalue.Check) knownvalue.Check {
	checks := map[string]knownvalue.Check{
		"endpoints": knownvalue.ObjectExact(map[string]knownvalue.Check{
			"dynamodb": knownvalue.StringExact("vpce-0d4e5f6a7b8c9d0e1"),
			"s3":       knownvalue.StringExact("vpce-0a1b2c3d4e5f6a7b8"),
		}),
		"nat_gateway_count": knownvalue.Int64Exact(3),
		"private_subnet_ids": knownvalue.ListExact([]knownvalue.Check{
			knownvalue.StringExact("subnet-0a1b2c3d4e5f6a7b8"),
			knownvalue.StringExact("subnet-0b2c3d4e5f6a7b8c9"),
		}),
		"tags": knownvalue.MapExact(map[string]knownvalue.Check{
			"environment": knownvalue.StringExact("prod"),
			"team":        knownvalue.StringExact("platform"),
		}),
		"vpc_id": knownvalue.StringExact("vpc-0a1b2c3d4e5f67890"),
	}
	maps.Copy(checks, extra)
	return knownvalue.ObjectExact(checks)
}

const vpcConfig = `
data "stackorder_outputs" "vpc" {
  stack = "stacks/prod/vpc"
}

output "vpc_id" {
  value = data.stackorder_outputs.vpc.nonsensitive_values.vpc_id
}

output "first_private_subnet" {
  value = data.stackorder_outputs.vpc.nonsensitive_values.private_subnet_ids[0]
}

output "s3_endpoint" {
  value     = data.stackorder_outputs.vpc.values.endpoints.s3
  sensitive = true
}
`

func TestAccOutputsDataSource(t *testing.T) {
	srv := newOutputsServer(t, map[string]served{vpcOutputsPath: okWith(golden(t, "stack_outputs.json"))})
	serverEnvironment(t, srv, map[string]string{"GITHUB_REPOSITORY": "acme/infra"})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: vpcConfig,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("stack"), knownvalue.StringExact(vpcStack)),
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("id"), knownvalue.StringExact(vpcID)),
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("values"), vpcCheck(nil)),
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("nonsensitive_values"), vpcCheck(nil)),
					statecheck.ExpectSensitiveValue(outputsAddress, tfjsonpath.New("values")),
					statecheck.ExpectKnownOutputValue("vpc_id", knownvalue.StringExact("vpc-0a1b2c3d4e5f67890")),
					statecheck.ExpectKnownOutputValue("first_private_subnet", knownvalue.StringExact("subnet-0a1b2c3d4e5f6a7b8")),
					statecheck.ExpectKnownOutputValue("s3_endpoint", knownvalue.StringExact("vpce-0a1b2c3d4e5f6a7b8")),
				},
			},
			{
				PreConfig: func() { srv.serve(vpcOutputsPath, okWith(sharingSensitive(t))) },
				Config: vpcConfig + `
output "vpn_preshared_key" {
  value     = data.stackorder_outputs.vpc.values.vpn_preshared_key
  sensitive = true
}
`,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("values"), vpcCheck(map[string]knownvalue.Check{"vpn_preshared_key": knownvalue.StringExact(pskCanary)})),
					statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("nonsensitive_values"), vpcCheck(nil)),
					statecheck.ExpectSensitiveValue(outputsAddress, tfjsonpath.New("values")),
					statecheck.ExpectKnownOutputValue("vpn_preshared_key", knownvalue.StringExact(pskCanary)),
				},
			},
		},
	})
}

func TestAccOutputsDataSourceCrossRepo(t *testing.T) {
	srv := newOutputsServer(t, map[string]served{clusterPath: okWith(encode(t, v1.StackOutputs{
		Repo:  "acme/platform",
		Stack: "clusters/prod",
		Outputs: map[string]v1.StackOutput{
			"cluster_name": {Value: json.RawMessage(`"prod"`), Type: json.RawMessage(`"string"`)},
		},
	}))})
	serverEnvironment(t, srv, map[string]string{"GITHUB_REPOSITORY": "acme/infra"})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: `
data "stackorder_outputs" "cluster" {
  stack = "acme/platform//clusters/prod"
}

output "cluster_name" {
  value = data.stackorder_outputs.cluster.nonsensitive_values.cluster_name
}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("data.stackorder_outputs.cluster", tfjsonpath.New("id"), knownvalue.StringExact(clusterStack)),
				statecheck.ExpectKnownValue("data.stackorder_outputs.cluster", tfjsonpath.New("values"), knownvalue.ObjectExact(map[string]knownvalue.Check{"cluster_name": knownvalue.StringExact("prod")})),
				statecheck.ExpectKnownOutputValue("cluster_name", knownvalue.StringExact("prod")),
			},
		}},
	})

	got := srv.requests()
	require.NotEmpty(t, got)
	for _, r := range got {
		assert.Equal(t, clusterPath, r.path)
	}
}

func TestAccOutputsDataSourceNotShared(t *testing.T) {
	srv := newOutputsServer(t, map[string]served{vpcOutputsPath: {
		status: http.StatusForbidden,
		body:   refusalBody(v1.ErrCodeOutputsNotShared, "stacks/prod/vpc does not share its outputs with stacks/prod/eks"),
	}})
	serverEnvironment(t, srv, map[string]string{"STACKORDER_REPOSITORY": "acme/infra", "STACKORDER_RUN_ID": testRunID, "STACKORDER_STACK": testReader})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config:      vpcConfig,
			ExpectError: regexp.MustCompile(`(?s)` + notSharedSummary + `.*- stacks/prod/eks.*answered\s+403\s+outputs_not_shared:\s+stacks/prod/vpc\s+does\s+not\s+share`),
		}},
	})
}

// plugin-testing cannot observe warnings, so TestOutputsDataSourceRead asserts the withheld warning.
func TestAccOutputsDataSourceWithheldWarns(t *testing.T) {
	srv := newOutputsServer(t, map[string]served{vpcOutputsPath: okWith(golden(t, "stack_outputs.json"))})
	serverEnvironment(t, srv, map[string]string{"GITHUB_REPOSITORY": "acme/infra"})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{{
			Config: vpcConfig + `
output "psk_served" {
  value     = contains(keys(data.stackorder_outputs.vpc.values), "vpn_preshared_key")
  sensitive = true
}
`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue(outputsAddress, tfjsonpath.New("values"), vpcCheck(nil)),
				statecheck.ExpectKnownOutputValue("psk_served", knownvalue.Bool(false)),
				statecheck.ExpectKnownOutputValue("vpc_id", knownvalue.StringExact("vpc-0a1b2c3d4e5f67890")),
			},
		}},
	})
}
