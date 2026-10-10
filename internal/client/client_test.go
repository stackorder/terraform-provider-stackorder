package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/stackorder/stackorder/api/v1"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
)

const (
	testRunID  = "5b1e3f0a-8c2d-4c1b-9a51-0d7f3e2a9b11"
	testAPIKey = "sk_acme_automation"
)

type reply struct {
	status int
	header http.Header
	body   string
	drop   bool
	cut    bool
	stall  bool
}

type seen struct {
	method    string
	path      string
	query     url.Values
	auth      string
	accept    string
	userAgent string
}

type stackorderServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []reply
	got     []seen
}

func fakeStackorder(t *testing.T, replies ...reply) *stackorderServer {
	t.Helper()
	s := &stackorderServer{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stackorderServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.got = append(s.got, seen{
		method:    r.Method,
		path:      r.URL.EscapedPath(),
		query:     r.URL.Query(),
		auth:      r.Header.Get("Authorization"),
		accept:    r.Header.Get("Accept"),
		userAgent: r.Header.Get("User-Agent"),
	})
	rep := s.replies[min(len(s.got), len(s.replies))-1]
	s.mu.Unlock()
	if rep.drop {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	for name, values := range rep.header {
		w.Header()[name] = values
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case rep.cut:
		w.Header().Set("Content-Length", strconv.Itoa(len(rep.body)+64))
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	case rep.stall:
		w.Header().Set("Content-Length", strconv.Itoa(len(rep.body)))
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body[:len(rep.body)/2]))
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	default:
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}
}

func (s *stackorderServer) requests() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.got...)
}

func (s *stackorderServer) auths() []string {
	got := s.requests()
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, r.auth)
	}
	return out
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func outputsReply(t *testing.T) reply {
	t.Helper()
	return reply{status: http.StatusOK, body: golden(t, "stack_outputs.json")}
}

func coded(status int, code, message string) reply {
	b, _ := json.Marshal(v1.Error{Code: code, Message: message})
	return reply{status: status, body: string(b)}
}

func newClient(t *testing.T, serverURL string, src client.TokenSource, opts ...client.Option) *client.Client {
	t.Helper()
	c, err := client.New(serverURL, src, append([]client.Option{
		client.WithVersion("1.2.3"),
		client.WithRetryPolicy(client.RetryPolicy{Attempts: client.DefaultAttempts}),
	}, opts...)...)
	require.NoError(t, err)
	return c
}

func apiKeySource(t *testing.T) client.TokenSource {
	t.Helper()
	outsideActions(t)
	src, err := client.NewTokenSource(client.Credentials{APIKey: testAPIKey})
	require.NoError(t, err)
	return src
}

func oidcSource(t *testing.T, serverURL string) (client.TokenSource, *actionsTokens) {
	t.Helper()
	actions := fakeActions(t)
	t.Setenv(client.EnvAPIKey, "")
	src, err := client.NewTokenSource(client.Credentials{ServerURL: serverURL})
	require.NoError(t, err)
	return src, actions
}

func TestOutputsRequestPath(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		repo     string
		stack    string
		runID    string
		reader   string
		wantPath string
		want     url.Values
	}{
		{
			name:     "stack instance read by a stack of a run",
			repo:     "acme/infra",
			stack:    "stacks/prod/apps:blue",
			runID:    testRunID,
			reader:   "stacks/prod/eks",
			wantPath: "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fapps:blue/outputs",
			want:     url.Values{"run": {testRunID}, "reader": {"stacks/prod/eks"}},
		},
		{
			name:     "stack read by a stack instance",
			repo:     "acme/infra",
			stack:    "stacks/prod/vpc",
			runID:    testRunID,
			reader:   "stacks/prod/apps:green",
			wantPath: "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fvpc/outputs",
			want:     url.Values{"run": {testRunID}, "reader": {"stacks/prod/apps:green"}},
		},
		{
			name:     "API key outside a run",
			repo:     "acme/network-infra",
			stack:    "stacks/prod/tgw:eu-west-1",
			wantPath: "/v1/repos/acme/network-infra/stacks/stacks%2Fprod%2Ftgw:eu-west-1/outputs",
			want:     url.Values{},
		},
		{
			name:     "server URL with a path prefix",
			prefix:   "/stackorder/",
			repo:     "acme/infra.live",
			stack:    "stacks/prod/vpc",
			runID:    testRunID,
			reader:   "stacks/prod/eks",
			wantPath: "/stackorder/v1/repos/acme/infra.live/stacks/stacks%2Fprod%2Fvpc/outputs",
			want:     url.Values{"run": {testRunID}, "reader": {"stacks/prod/eks"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, outputsReply(t))
			c := newClient(t, s.srv.URL+tt.prefix, apiKeySource(t))

			_, err := c.StackOutputs(t.Context(), tt.repo, tt.stack, tt.runID, tt.reader)
			require.NoError(t, err)

			got := s.requests()
			require.Len(t, got, 1)
			assert.Equal(t, http.MethodGet, got[0].method)
			assert.Equal(t, tt.wantPath, got[0].path)
			assert.Equal(t, tt.want, got[0].query)
			assert.Equal(t, "application/json", got[0].accept)
			assert.Equal(t, "Bearer "+testAPIKey, got[0].auth)
			assert.Equal(t, "terraform-provider-stackorder/1.2.3", got[0].userAgent)
		})
	}
}

func TestOIDCTokenPerAttempt(t *testing.T) {
	tests := []struct {
		name       string
		replies    []reply
		wantStatus int
	}{
		{
			name: "every attempt fails",
			replies: []reply{
				coded(http.StatusServiceUnavailable, "unavailable", "state read throttled"),
				coded(http.StatusServiceUnavailable, "unavailable", "state read throttled"),
				coded(http.StatusServiceUnavailable, "unavailable", "state read throttled"),
			},
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "last attempt succeeds",
			replies: []reply{
				{status: http.StatusBadGateway, body: "<html>bad gateway</html>"},
				coded(http.StatusTooManyRequests, "rate_limited", "slow down"),
				outputsReply(t),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, tt.replies...)
			src, actions := oidcSource(t, s.srv.URL)
			c := newClient(t, s.srv.URL, src)

			out, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", testRunID, "stacks/prod/eks")

			assert.Equal(t, []string{"Bearer oidc-token-1", "Bearer oidc-token-2", "Bearer oidc-token-3"}, s.auths())
			assert.Equal(t, []string{s.srv.URL, s.srv.URL, s.srv.URL}, actions.minted())
			if tt.wantStatus != 0 {
				var apiErr *client.APIError
				require.ErrorAs(t, err, &apiErr)
				assert.Equal(t, tt.wantStatus, apiErr.Status)
				assert.Contains(t, err.Error(), "giving up after 3 attempts")
				assert.NotContains(t, err.Error(), "oidc-token")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "stacks/prod/vpc", out.Stack)
		})
	}
}

func TestRetriesWithFreshToken(t *testing.T) {
	tests := []struct {
		name          string
		tokenFailures int
		opts          []client.Option
		replies       []reply
		wantAuths     []string
	}{
		{
			name:      "503 then 200",
			replies:   []reply{coded(http.StatusServiceUnavailable, "unavailable", "try again"), outputsReply(t)},
			wantAuths: []string{"Bearer oidc-token-1", "Bearer oidc-token-2"},
		},
		{
			name:      "429 then 200",
			replies:   []reply{{status: http.StatusTooManyRequests}, outputsReply(t)},
			wantAuths: []string{"Bearer oidc-token-1", "Bearer oidc-token-2"},
		},
		{
			name:      "dropped connection then 200",
			replies:   []reply{{drop: true}, outputsReply(t)},
			wantAuths: []string{"Bearer oidc-token-1", "Bearer oidc-token-2"},
		},
		{
			name:      "connection lost while reading the answer then 200",
			replies:   []reply{{status: http.StatusOK, body: golden(t, "stack_outputs.json"), cut: true}, outputsReply(t)},
			wantAuths: []string{"Bearer oidc-token-1", "Bearer oidc-token-2"},
		},
		{
			name:      "timeout while reading the answer then 200",
			opts:      []client.Option{client.WithHTTPClient(&http.Client{Timeout: 300 * time.Millisecond})},
			replies:   []reply{{status: http.StatusOK, body: golden(t, "stack_outputs.json"), stall: true}, outputsReply(t)},
			wantAuths: []string{"Bearer oidc-token-1", "Bearer oidc-token-2"},
		},
		{
			name:          "ID token endpoint unavailable once",
			tokenFailures: 1,
			replies:       []reply{outputsReply(t)},
			wantAuths:     []string{"Bearer oidc-token-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, tt.replies...)
			src, actions := oidcSource(t, s.srv.URL)
			actions.failNext(tt.tokenFailures)
			c := newClient(t, s.srv.URL, src, tt.opts...)

			out, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", testRunID, "stacks/prod/eks")
			require.NoError(t, err)
			assert.Equal(t, "stacks/prod/vpc", out.Stack)
			assert.Equal(t, tt.wantAuths, s.auths())
			assert.Len(t, actions.minted(), len(tt.wantAuths))
		})
	}
}

func TestNoRetryOnCodedRefusals(t *testing.T) {
	tests := []struct {
		name        string
		reply       reply
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "reader not listed",
			reply:       coded(http.StatusForbidden, v1.ErrCodeOutputsNotShared, "stacks/prod/eks is not a reader of stacks/prod/vpc"),
			wantStatus:  http.StatusForbidden,
			wantCode:    v1.ErrCodeOutputsNotShared,
			wantMessage: "stacks/prod/eks is not a reader of stacks/prod/vpc",
		},
		{
			name:        "feature off",
			reply:       coded(http.StatusNotFound, v1.ErrCodeOutputsDisabled, "shared outputs are disabled"),
			wantStatus:  http.StatusNotFound,
			wantCode:    v1.ErrCodeOutputsDisabled,
			wantMessage: "shared outputs are disabled",
		},
		{
			name:        "another repository",
			reply:       coded(http.StatusNotFound, "not_found", "stack not found"),
			wantStatus:  http.StatusNotFound,
			wantCode:    "not_found",
			wantMessage: "stack not found",
		},
		{
			name:        "state location unknown",
			reply:       coded(http.StatusConflict, v1.ErrCodeStateLocationUnknown, "no apply or drift job has reported the state object"),
			wantStatus:  http.StatusConflict,
			wantCode:    v1.ErrCodeStateLocationUnknown,
			wantMessage: "no apply or drift job has reported the state object",
		},
		{
			name:        "encrypted state",
			reply:       coded(http.StatusUnprocessableEntity, v1.ErrCodeStateEncrypted, "the state is encrypted"),
			wantStatus:  http.StatusUnprocessableEntity,
			wantCode:    v1.ErrCodeStateEncrypted,
			wantMessage: "the state is encrypted",
		},
		{
			name:        "token refused",
			reply:       coded(http.StatusUnauthorized, "unauthorized", "token already used"),
			wantStatus:  http.StatusUnauthorized,
			wantCode:    "unauthorized",
			wantMessage: "token already used",
		},
		{
			name:       "code without a message",
			reply:      coded(http.StatusForbidden, "forbidden", ""),
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:        "proxy page",
			reply:       reply{status: http.StatusForbidden, body: "<html>\n  <body>Forbidden</body>\n</html>\n"},
			wantStatus:  http.StatusForbidden,
			wantMessage: "<html> <body>Forbidden</body> </html>",
		},
		{
			name:        "long proxy page cut at a character boundary",
			reply:       reply{status: http.StatusForbidden, body: "x" + strings.Repeat("é", 150)},
			wantStatus:  http.StatusForbidden,
			wantMessage: "x" + strings.Repeat("é", 99) + "…",
		},
		{
			name:        "empty body",
			reply:       reply{status: http.StatusBadRequest},
			wantStatus:  http.StatusBadRequest,
			wantMessage: "Bad Request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, tt.reply, outputsReply(t))
			src, actions := oidcSource(t, s.srv.URL)
			c := newClient(t, s.srv.URL, src)

			out, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", testRunID, "stacks/prod/eks")
			assert.Nil(t, out)

			var apiErr *client.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, client.APIError{Status: tt.wantStatus, Code: tt.wantCode, Message: tt.wantMessage}, *apiErr)
			assert.Equal(t, "reading the outputs of acme/infra//stacks/prod/vpc: "+apiErr.Error(), err.Error())
			assert.NotContains(t, err.Error(), "oidc-token")
			assert.Len(t, s.requests(), 1)
			assert.Len(t, actions.minted(), 1)
		})
	}
}

func TestDecodesStackOutputs(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    *v1.StackOutputs
		wantErr string
	}{
		{
			name: "shared outputs",
			body: golden(t, "stack_outputs.json"),
			want: &v1.StackOutputs{
				Repo:             "acme/infra",
				Stack:            "stacks/prod/vpc",
				Serial:           42,
				Lineage:          "8d3f6c1e-2b7a-4e9d-9c51-0f4a6b2d7e13",
				TerraformVersion: "1.9.8",
				Outputs: map[string]v1.StackOutput{
					"vpc_id": {
						Value: json.RawMessage(`"vpc-0a1b2c3d4e5f67890"`),
						Type:  json.RawMessage(`"string"`),
					},
					"nat_gateway_count": {
						Value: json.RawMessage(`3`),
						Type:  json.RawMessage(`"number"`),
					},
					"private_subnet_ids": {
						Value: json.RawMessage(`["subnet-0a1b2c3d4e5f6a7b8","subnet-0b2c3d4e5f6a7b8c9"]`),
						Type:  json.RawMessage(`["list","string"]`),
					},
					"tags": {
						Value: json.RawMessage(`{"environment":"prod","team":"platform"}`),
						Type:  json.RawMessage(`["map","string"]`),
					},
					"endpoints": {
						Value: json.RawMessage(`{"dynamodb":"vpce-0d4e5f6a7b8c9d0e1","s3":"vpce-0a1b2c3d4e5f6a7b8"}`),
						Type:  json.RawMessage(`["object",{"dynamodb":"string","s3":"string"}]`),
					},
				},
				Withheld: []string{"vpn_preshared_key"},
			},
		},
		{
			name: "no outputs",
			body: golden(t, "stack_outputs_empty.json"),
			want: &v1.StackOutputs{
				Repo:             "acme/infra",
				Stack:            "stacks/prod/dns",
				Serial:           3,
				Lineage:          "5b2e9a74-1c3d-4f60-8a7e-2d9c0b1f6e48",
				TerraformVersion: "1.9.8",
				Outputs:          map[string]v1.StackOutput{},
			},
		},
		{
			name:    "truncated body",
			body:    `{"repo":"acme/infra","outputs":{`,
			wantErr: "reading the outputs of acme/infra//stacks/prod/vpc: decoding the response: ",
		},
		{
			name:    "field of the wrong type",
			body:    `{"repo":"acme/infra","stack":"stacks/prod/vpc","serial":"42","outputs":{}}`,
			wantErr: "reading the outputs of acme/infra//stacks/prod/vpc: decoding the response: ",
		},
		{
			name:    "body over 32 MiB",
			body:    `{"repo":"acme/infra","outputs":{}}` + strings.Repeat(" ", 32<<20),
			wantErr: "reading the outputs of acme/infra//stacks/prod/vpc: the response is larger than 32 MiB",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, reply{status: http.StatusOK, body: tt.body})
			c := newClient(t, s.srv.URL, apiKeySource(t))

			got, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", "", "")
			assert.Len(t, s.requests(), 1)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			for name, out := range got.Outputs {
				out.Value, out.Type = compact(t, out.Value), compact(t, out.Type)
				got.Outputs[name] = out
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRetriesStopWhenTheContextEnds(t *testing.T) {
	s := fakeStackorder(t, coded(http.StatusServiceUnavailable, "unavailable", "try again"))
	c, err := client.New(s.srv.URL, apiKeySource(t), client.WithRetryPolicy(client.RetryPolicy{
		Attempts: client.DefaultAttempts,
		Backoff:  []time.Duration{time.Hour},
	}))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	out, err := c.StackOutputs(ctx, "acme/infra", "stacks/prod/vpc", "", "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, out)
	assert.Len(t, s.requests(), 1)
}

func TestRequestsGoThroughTheGivenHTTPClient(t *testing.T) {
	tests := []struct {
		name     string
		nilAfter bool
	}{
		{name: "given client"},
		{name: "nil keeps the client given before", nilAfter: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, outputsReply(t))
			var used atomic.Int32
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				used.Add(1)
				return http.DefaultTransport.RoundTrip(r)
			})}
			opts := []client.Option{client.WithHTTPClient(hc)}
			if tt.nilAfter {
				opts = append(opts, client.WithHTTPClient(nil))
			}
			c, err := client.New(s.srv.URL, apiKeySource(t), opts...)
			require.NoError(t, err)

			_, err = c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", "", "")
			require.NoError(t, err)
			assert.Equal(t, int32(1), used.Load())
			assert.Len(t, s.requests(), 1)
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	tests := []struct {
		name string
		opts []client.Option
	}{
		{name: "default client"},
		{name: "given client", opts: []client.Option{client.WithHTTPClient(&http.Client{})}},
		{name: "nil client", opts: []client.Option{client.WithHTTPClient(nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elsewhere := fakeStackorder(t, outputsReply(t))
			location := elsewhere.srv.URL + "/v1/repos/acme/infra/stacks/stacks%2Fprod%2Fvpc/outputs"
			s := fakeStackorder(t, reply{
				status: http.StatusFound,
				header: http.Header{"Location": {location}},
				body:   `<a href="` + location + `">Found</a>.`,
			})
			c := newClient(t, s.srv.URL, apiKeySource(t), tt.opts...)

			out, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", "", "")
			assert.Nil(t, out)
			var apiErr *client.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, client.APIError{Status: http.StatusFound, Message: "redirected to " + location + ", which is never followed"}, *apiErr)
			assert.Len(t, s.requests(), 1)
			assert.Empty(t, elsewhere.requests())
		})
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		name         string
		first        reply
		wantRequests int
		wantErr      string
	}{
		{
			name:         "seconds waited for",
			first:        reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"1"}}},
			wantRequests: 1,
			wantErr:      "context deadline exceeded",
		},
		{
			name:         "date waited for",
			first:        reply{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)}}},
			wantRequests: 1,
			wantErr:      "context deadline exceeded",
		},
		{
			name:         "zero seconds",
			first:        reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"0"}}},
			wantRequests: 2,
		},
		{
			name:         "date in the past",
			first:        reply{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)}}},
			wantRequests: 2,
		},
		{
			name:         "unparsable header",
			first:        reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"soon"}}},
			wantRequests: 2,
		},
		{
			name:         "more than a minute",
			first:        reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"3600"}}},
			wantRequests: 1,
			wantErr:      "not retrying, the answer asks to wait 1h0m0s",
		},
		{
			name:         "more seconds than fit",
			first:        reply{status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"99999999999999999999"}}},
			wantRequests: 1,
			wantErr:      "not retrying, the answer asks to wait",
		},
		{
			name:         "date more than a minute away",
			first:        reply{status: http.StatusServiceUnavailable, header: http.Header{"Retry-After": {time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)}}},
			wantRequests: 1,
			wantErr:      "not retrying, the answer asks to wait",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, tt.first, outputsReply(t))
			c := newClient(t, s.srv.URL, apiKeySource(t))
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()

			out, err := c.StackOutputs(ctx, "acme/infra", "stacks/prod/vpc", "", "")
			assert.Len(t, s.requests(), tt.wantRequests)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "stacks/prod/vpc", out.Stack)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, out)
			var apiErr *client.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tt.first.status, apiErr.Status)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNewRefusesInvalidSettings(t *testing.T) {
	tests := []struct {
		name      string
		serverURL string
		noSource  bool
		wantErr   string
	}{
		{name: "empty URL", serverURL: "", wantErr: "the Stackorder server URL is not an http or https URL with a host"},
		{name: "no scheme", serverURL: "stackorder.acme.example", wantErr: "is not an http or https URL with a host"},
		{name: "other scheme", serverURL: "ftp://stackorder.acme.example", wantErr: "is not an http or https URL with a host"},
		{name: "no host", serverURL: "https://", wantErr: "is not an http or https URL with a host"},
		{name: "unparsable URL", serverURL: "https://stackorder.acme.example/%zz", wantErr: "the Stackorder server URL does not parse as a URL"},
		{name: "unparsable URL with user info", serverURL: "https://ci:s3cr3t@stackorder.acme.example/%zz", wantErr: "does not parse as a URL"},
		{name: "password read as a port", serverURL: "https://ci:s3cr3t", wantErr: "does not parse as a URL"},
		{name: "other scheme with user info", serverURL: "ftp://ci:s3cr3t@stackorder.acme.example", wantErr: "is not an http or https URL with a host"},
		{name: "user info without a scheme", serverURL: "ci:s3cr3t@stackorder.acme.example", wantErr: "is not an http or https URL with a host"},
		{name: "user info", serverURL: "https://ci:s3cr3t@stackorder.acme.example", wantErr: "the Stackorder server URL carries user info, a query or a fragment"},
		{name: "query", serverURL: "https://stackorder.acme.example/?tenant=acme", wantErr: "carries user info, a query or a fragment"},
		{name: "secret in the query", serverURL: "https://stackorder.acme.example/?key=s3cr3t", wantErr: "carries user info, a query or a fragment"},
		{name: "empty query", serverURL: "https://stackorder.acme.example/?", wantErr: "carries user info, a query or a fragment"},
		{name: "fragment", serverURL: "https://stackorder.acme.example/#acme", wantErr: "carries user info, a query or a fragment"},
		{name: "empty fragment", serverURL: "https://stackorder.acme.example/#", wantErr: "carries user info, a query or a fragment"},
		{name: "no token source", serverURL: "https://stackorder.acme.example", noSource: true, wantErr: "no token source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := apiKeySource(t)
			if tt.noSource {
				src = nil
			}
			c, err := client.New(tt.serverURL, src)
			require.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), "s3cr3t")
			assert.NotContains(t, err.Error(), "acme")
			assert.Nil(t, c)
		})
	}
}

func TestInvalidArgumentsSendNothing(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		stack   string
		wantErr string
	}{
		{name: "repository without owner", repo: "infra", stack: "stacks/prod/vpc", wantErr: `the repository "infra" is not owner/repo`},
		{name: "qualified stack key as repository", repo: "acme/infra//stacks/prod/vpc", stack: "stacks/prod/vpc", wantErr: "is not owner/repo"},
		{name: "empty owner", repo: "/infra", stack: "stacks/prod/vpc", wantErr: "is not owner/repo"},
		{name: "empty name", repo: "acme/", stack: "stacks/prod/vpc", wantErr: "is not owner/repo"},
		{name: "dot segment", repo: "acme/..", stack: "stacks/prod/vpc", wantErr: "is not owner/repo"},
		{name: "segment that needs escaping", repo: "acme/in fra", stack: "stacks/prod/vpc", wantErr: "is not owner/repo"},
		{name: "empty stack", repo: "acme/infra", stack: "", wantErr: "the stack key is empty"},
		{name: "dot stack", repo: "acme/infra", stack: ".", wantErr: `the stack key "." is not a stack`},
		{name: "dot-dot stack", repo: "acme/infra", stack: "..", wantErr: `the stack key ".." is not a stack`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fakeStackorder(t, outputsReply(t))
			src, actions := oidcSource(t, s.srv.URL)
			c := newClient(t, s.srv.URL, src)

			out, err := c.StackOutputs(t.Context(), tt.repo, tt.stack, testRunID, "stacks/prod/eks")
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, out)
			assert.Empty(t, s.requests())
			assert.Empty(t, actions.minted())
		})
	}
}

func compact(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, raw))
	return buf.Bytes()
}
