package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvOIDCRequestURL is the runner variable holding the GitHub Actions ID
	// token endpoint. GitHub sets it only for jobs granted
	// `permissions: id-token: write`.
	EnvOIDCRequestURL = "ACTIONS_ID_TOKEN_REQUEST_URL"
	// EnvOIDCRequestToken is the runner variable holding the bearer token for
	// the ID token endpoint.
	EnvOIDCRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"
	// EnvOIDCAudience overrides the audience of the ID tokens when
	// Credentials.OIDCAudience is empty.
	EnvOIDCAudience = "STACKORDER_OIDC_AUDIENCE"
	// EnvAPIKey holds the automation API key used when Credentials.APIKey is
	// empty.
	EnvAPIKey = "STACKORDER_API_KEY" //nolint:gosec // a variable name, not a secret

	tokenRequestTimeout = 30 * time.Second
)

var (
	// ErrNoCredentials reports that the provider has neither a GitHub
	// Actions ID token nor an API key to authenticate with.
	ErrNoCredentials = errors.New("no Stackorder credentials: inside GitHub Actions, grant the job `permissions: id-token: write`; elsewhere, set the provider's api_key or " + EnvAPIKey)
	// ErrNoAudience reports that inside GitHub Actions nothing names the
	// audience of the ID tokens.
	ErrNoAudience = errors.New("no audience for the GitHub Actions ID tokens: set the provider's server_url or oidc_audience, or " + EnvOIDCAudience)
)

// TokenSource supplies the bearer token of each request attempt.
type TokenSource interface {
	// Token returns the token to send with one attempt.
	Token(ctx context.Context) (string, error)
}

// Credentials are the settings NewTokenSource chooses a TokenSource from.
// Every setting is trimmed of surrounding whitespace.
type Credentials struct {
	// ServerURL is the server URL as configured. Unless OIDCAudience or
	// STACKORDER_OIDC_AUDIENCE is set it is the audience of the ID tokens,
	// trailing slash included, and the server's default audience is its
	// base URL without one.
	ServerURL string
	// OIDCAudience is the audience of the ID tokens.
	OIDCAudience string
	// APIKey is an automation API key (sk_…). When empty, the key comes
	// from STACKORDER_API_KEY.
	APIKey string
}

// NewTokenSource returns a source that requests a new GitHub Actions ID
// token for every attempt whenever ACTIONS_ID_TOKEN_REQUEST_URL and
// ACTIONS_ID_TOKEN_REQUEST_TOKEN are set, so that an API key never silently
// takes precedence inside GitHub Actions, and ErrNoAudience when no setting
// names the tokens' audience. Otherwise it returns a source that sends the
// configured API key, or ErrNoCredentials when there is none. The ID token
// requests never follow redirects; network failures, 429 and 5xx answers
// of the token endpoint are retried like those of the server. No format
// verb prints a source's secret.
func NewTokenSource(creds Credentials) (TokenSource, error) {
	requestURL := strings.TrimSpace(os.Getenv(EnvOIDCRequestURL))
	requestToken := strings.TrimSpace(os.Getenv(EnvOIDCRequestToken))
	if requestURL != "" && requestToken != "" {
		audience := firstNonEmpty(creds.OIDCAudience, os.Getenv(EnvOIDCAudience), creds.ServerURL)
		if audience == "" {
			return nil, ErrNoAudience
		}
		return &oidcSource{
			requestURL:   requestURL,
			requestToken: requestToken,
			audience:     audience,
			http:         &http.Client{Timeout: tokenRequestTimeout, CheckRedirect: noRedirects},
		}, nil
	}
	if key := firstNonEmpty(creds.APIKey, os.Getenv(EnvAPIKey)); key != "" {
		return apiKeySource(key), nil
	}
	return nil, ErrNoCredentials
}

type oidcSource struct {
	requestURL   string
	requestToken string
	audience     string
	http         *http.Client
}

// Token requests an ID token for the source's audience.
func (s *oidcSource) Token(ctx context.Context) (string, error) {
	sep := "?"
	if strings.Contains(s.requestURL, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.requestURL+sep+"audience="+url.QueryEscape(s.audience), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("building the GitHub Actions ID token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.requestToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", retryable(fmt.Errorf("requesting a GitHub Actions ID token: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return "", retryable(fmt.Errorf("reading the GitHub Actions ID token: %w", err))
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("the GitHub Actions ID token endpoint answered %d: %s", resp.StatusCode, snippet(body))
		if transientStatus(resp.StatusCode) {
			return "", transient(err, resp.Header)
		}
		return "", err
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decoding the GitHub Actions ID token response: %w", err)
	}
	if out.Value == "" {
		return "", errors.New("the GitHub Actions ID token endpoint returned no token")
	}
	return out.Value, nil
}

// String names the source and its audience, never its request token.
func (s *oidcSource) String() string {
	return "GitHub Actions ID tokens for audience " + strconv.Quote(s.audience)
}

// Format prints String whatever the verb, so no verb shows the request token.
func (s *oidcSource) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, s.String()) }

type apiKeySource string

// Token returns the API key.
func (k apiKeySource) Token(context.Context) (string, error) { return string(k), nil }

// String names the source, never its key.
func (apiKeySource) String() string { return "automation API key" }

// Format prints String whatever the verb, so no verb shows the key.
func (k apiKeySource) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
