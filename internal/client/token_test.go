package client_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stackorder/terraform-provider-stackorder/internal/client"
)

const actionsRequestToken = "actions-request-token"

type actionsTokens struct {
	mu        sync.Mutex
	failures  int
	audiences []string
}

func fakeActions(t *testing.T) *actionsTokens {
	t.Helper()
	a := &actionsTokens{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+actionsRequestToken || r.URL.Query().Get("api-version") != "2.0" {
			http.Error(w, `{"message":"bad token request"}`, http.StatusUnauthorized)
			return
		}
		a.mu.Lock()
		if a.failures > 0 {
			a.failures--
			a.mu.Unlock()
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		a.audiences = append(a.audiences, r.URL.Query().Get("audience"))
		n := len(a.audiences)
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "value": "oidc-token-" + strconv.Itoa(n)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv(client.EnvOIDCRequestURL, srv.URL+"/idtoken?api-version=2.0")
	t.Setenv(client.EnvOIDCRequestToken, actionsRequestToken)
	t.Setenv(client.EnvOIDCAudience, "")
	return a
}

func (a *actionsTokens) failNext(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = n
}

func (a *actionsTokens) minted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.audiences...)
}

func outsideActions(t *testing.T) {
	t.Helper()
	for _, name := range []string{client.EnvOIDCRequestURL, client.EnvOIDCRequestToken, client.EnvOIDCAudience, client.EnvAPIKey} {
		t.Setenv(name, "")
	}
}

func TestOIDCPreferredInActions(t *testing.T) {
	tests := []struct {
		name         string
		creds        client.Credentials
		envAudience  string
		envAPIKey    string
		wantAudience string
		wantErr      error
	}{
		{
			name:         "server URL exactly as given",
			creds:        client.Credentials{ServerURL: "https://stackorder.acme.example/"},
			wantAudience: "https://stackorder.acme.example/",
		},
		{
			name:         "audience from the environment",
			creds:        client.Credentials{ServerURL: "https://stackorder.acme.example"},
			envAudience:  "stackorder-acme",
			wantAudience: "stackorder-acme",
		},
		{
			name:         "configured audience wins over the environment",
			creds:        client.Credentials{ServerURL: "https://stackorder.acme.example", OIDCAudience: "stackorder-configured"},
			envAudience:  "stackorder-acme",
			wantAudience: "stackorder-configured",
		},
		{
			name:         "configured API key ignored",
			creds:        client.Credentials{ServerURL: "https://stackorder.acme.example", APIKey: "sk_configured"},
			wantAudience: "https://stackorder.acme.example",
		},
		{
			name:         "API key from the environment ignored",
			creds:        client.Credentials{ServerURL: "https://stackorder.acme.example"},
			envAPIKey:    "sk_environment",
			wantAudience: "https://stackorder.acme.example",
		},
		{
			name:         "surrounding whitespace trimmed",
			creds:        client.Credentials{ServerURL: " https://stackorder.acme.example\n"},
			wantAudience: "https://stackorder.acme.example",
		},
		{
			name:    "no audience",
			creds:   client.Credentials{APIKey: "sk_configured"},
			wantErr: client.ErrNoAudience,
		},
		{
			name:        "blank audience settings",
			creds:       client.Credentials{ServerURL: " ", OIDCAudience: "\t"},
			envAudience: "\n",
			wantErr:     client.ErrNoAudience,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actions := fakeActions(t)
			t.Setenv(client.EnvOIDCAudience, tt.envAudience)
			t.Setenv(client.EnvAPIKey, tt.envAPIKey)

			src, err := client.NewTokenSource(tt.creds)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, src)
				assert.Contains(t, err.Error(), "server_url")
				assert.Contains(t, err.Error(), "oidc_audience")
				assert.Contains(t, err.Error(), client.EnvOIDCAudience)
				assert.Empty(t, actions.minted())
				return
			}
			require.NoError(t, err)

			tok, err := src.Token(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "oidc-token-1", tok)
			assert.Equal(t, []string{tt.wantAudience}, actions.minted())
		})
	}
}

func TestAPIKeyOutsideActions(t *testing.T) {
	tests := []struct {
		name            string
		creds           client.Credentials
		envAPIKey       string
		envRequestURL   string
		envRequestToken string
		want            string
	}{
		{
			name:  "configured key",
			creds: client.Credentials{APIKey: "sk_configured"},
			want:  "sk_configured",
		},
		{
			name:      "key from the environment",
			envAPIKey: "sk_environment",
			want:      "sk_environment",
		},
		{
			name:      "configured key wins over the environment",
			creds:     client.Credentials{APIKey: "sk_configured"},
			envAPIKey: "sk_environment",
			want:      "sk_configured",
		},
		{
			name:      "surrounding whitespace trimmed",
			envAPIKey: " sk_environment\n",
			want:      "sk_environment",
		},
		{
			name:          "ID token endpoint without its request token",
			creds:         client.Credentials{APIKey: "sk_configured"},
			envRequestURL: "https://actions.example/idtoken?api-version=2.0",
			want:          "sk_configured",
		},
		{
			name:            "request token without the ID token endpoint",
			creds:           client.Credentials{APIKey: "sk_configured"},
			envRequestToken: actionsRequestToken,
			want:            "sk_configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outsideActions(t)
			t.Setenv(client.EnvAPIKey, tt.envAPIKey)
			t.Setenv(client.EnvOIDCRequestURL, tt.envRequestURL)
			t.Setenv(client.EnvOIDCRequestToken, tt.envRequestToken)

			src, err := client.NewTokenSource(tt.creds)
			require.NoError(t, err)

			tok, err := src.Token(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, tok)
		})
	}
}

func TestNoCredentials(t *testing.T) {
	tests := []struct {
		name            string
		creds           client.Credentials
		envAPIKey       string
		envRequestURL   string
		envRequestToken string
	}{
		{
			name:  "nothing configured",
			creds: client.Credentials{ServerURL: "https://stackorder.acme.example"},
		},
		{
			name:      "blank API keys",
			creds:     client.Credentials{ServerURL: "https://stackorder.acme.example", APIKey: "  "},
			envAPIKey: "\n",
		},
		{
			name:          "ID token endpoint without its request token",
			envRequestURL: "https://actions.example/idtoken?api-version=2.0",
		},
		{
			name:            "request token without the ID token endpoint",
			envRequestToken: actionsRequestToken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outsideActions(t)
			t.Setenv(client.EnvAPIKey, tt.envAPIKey)
			t.Setenv(client.EnvOIDCRequestURL, tt.envRequestURL)
			t.Setenv(client.EnvOIDCRequestToken, tt.envRequestToken)

			src, err := client.NewTokenSource(tt.creds)
			require.ErrorIs(t, err, client.ErrNoCredentials)
			assert.Nil(t, src)
			assert.Contains(t, err.Error(), "id-token: write")
			assert.Contains(t, err.Error(), "api_key")
			assert.Contains(t, err.Error(), client.EnvAPIKey)
		})
	}
}

func TestOIDCTokenEndpointFailures(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		redirect     bool
		body         string
		wantRequests int
		wantErr      string
	}{
		{
			name:         "redirected",
			status:       http.StatusTemporaryRedirect,
			redirect:     true,
			wantRequests: 1,
			wantErr:      "obtaining a token: the GitHub Actions ID token endpoint answered 307",
		},
		{
			name:         "refused",
			status:       http.StatusForbidden,
			body:         `{"message":"Can't issue ID_TOKEN"}`,
			wantRequests: 1,
			wantErr:      `obtaining a token: the GitHub Actions ID token endpoint answered 403: {"message":"Can't issue ID_TOKEN"}`,
		},
		{
			name:         "unavailable",
			status:       http.StatusServiceUnavailable,
			body:         "unavailable",
			wantRequests: client.DefaultAttempts,
			wantErr:      "giving up after 3 attempts: obtaining a token: the GitHub Actions ID token endpoint answered 503: unavailable",
		},
		{
			name:         "not JSON",
			status:       http.StatusOK,
			body:         "<html>",
			wantRequests: 1,
			wantErr:      "obtaining a token: decoding the GitHub Actions ID token response",
		},
		{
			name:         "no token",
			status:       http.StatusOK,
			body:         `{"count":0}`,
			wantRequests: 1,
			wantErr:      "obtaining a token: the GitHub Actions ID token endpoint returned no token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests, redirected atomic.Int32
			elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirected.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"value": "oidc-token-elsewhere"})
			}))
			t.Cleanup(elsewhere.Close)
			actions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tt.redirect {
					w.Header().Set("Location", elsewhere.URL+r.URL.RequestURI())
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(actions.Close)
			outsideActions(t)
			t.Setenv(client.EnvOIDCRequestURL, actions.URL+"/idtoken?api-version=2.0")
			t.Setenv(client.EnvOIDCRequestToken, actionsRequestToken)
			s := fakeStackorder(t, outputsReply(t))
			src, err := client.NewTokenSource(client.Credentials{ServerURL: s.srv.URL})
			require.NoError(t, err)
			c := newClient(t, s.srv.URL, src)

			out, err := c.StackOutputs(t.Context(), "acme/infra", "stacks/prod/vpc", testRunID, "stacks/prod/eks")
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, out)
			assert.NotContains(t, err.Error(), actionsRequestToken)
			assert.Equal(t, int32(tt.wantRequests), requests.Load())
			assert.Zero(t, redirected.Load())
			assert.Empty(t, s.requests())
		})
	}
}

func TestTokenSourcesHideSecrets(t *testing.T) {
	tests := []struct {
		name   string
		inside bool
		secret string
		want   string
	}{
		{
			name:   "ID token source",
			inside: true,
			secret: actionsRequestToken,
			want:   `GitHub Actions ID tokens for audience "https://stackorder.acme.example"`,
		},
		{
			name:   "API key source",
			secret: testAPIKey,
			want:   "automation API key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outsideActions(t)
			if tt.inside {
				fakeActions(t)
			}
			src, err := client.NewTokenSource(client.Credentials{ServerURL: "https://stackorder.acme.example", APIKey: testAPIKey})
			require.NoError(t, err)

			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%T %[1]v"} {
				got := fmt.Sprintf(verb, src)
				assert.NotContains(t, got, tt.secret, verb)
				assert.Contains(t, got, tt.want, verb)
			}
			assert.Equal(t, tt.want, fmt.Sprint(src))
		})
	}
}
