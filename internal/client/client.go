// Package client reads a stack's shared outputs from the Stackorder server.
//
// Inside GitHub Actions every request attempt carries a new OIDC ID token,
// because the server accepts each token only once; elsewhere it carries an
// automation API key. Network failures, a connection lost while the answer
// is read included, and 429 and 5xx answers are retried; any other answer is
// final and returned as an *APIError whose Code is the server's v1.Error
// code, so the provider can turn each refusal into its own diagnostic.
// Redirects are never followed, so a token only ever reaches the configured
// server.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "github.com/stackorder/stackorder/api/v1"
)

const (
	// DefaultAttempts is how many requests a call sends at most, the first
	// included, unless WithRetryPolicy says otherwise.
	DefaultAttempts = 3
	// DefaultTimeout bounds each request unless WithHTTPClient replaces the
	// HTTP client.
	DefaultTimeout = 30 * time.Second

	maxRetryAfter   = time.Minute
	maxResponseBody = 32 << 20
)

// RetryPolicy says how often a call is retried after a network failure, a
// 429 or a 5xx answer, and how long it waits in between. A Retry-After
// header on the answer lengthens the wait to what it asks for; an answer
// that asks for more than a minute is final.
type RetryPolicy struct {
	// Attempts is how many requests a call sends at most, the first
	// included. Values below 1 mean 1.
	Attempts int
	// Backoff lists the waits before the second, third and later attempts.
	// The last wait repeats; an empty list retries at once.
	Backoff []time.Duration
}

func (p RetryPolicy) wait(retry int) time.Duration {
	if len(p.Backoff) == 0 {
		return 0
	}
	return p.Backoff[min(retry, len(p.Backoff))-1]
}

// Client reads stack outputs from one Stackorder server. It is safe for
// concurrent use.
type Client struct {
	baseURL   string
	tokens    TokenSource
	http      *http.Client
	userAgent string
	retry     RetryPolicy
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sends requests through a copy of hc that never follows
// redirects. A nil hc keeps the client set before. The ID token requests of
// a NewTokenSource source use their own HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.http = withoutRedirects(hc)
		}
	}
}

// WithVersion sets the provider version reported in the User-Agent header,
// terraform-provider-stackorder/<version>. The default version is "dev".
func WithVersion(version string) Option {
	return func(c *Client) {
		if version != "" {
			c.userAgent = userAgent(version)
		}
	}
}

// WithRetryPolicy replaces the default policy of DefaultAttempts attempts,
// 500ms then 2s apart.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *Client) {
		c.retry = RetryPolicy{Attempts: max(p.Attempts, 1), Backoff: append([]time.Duration(nil), p.Backoff...)}
	}
}

// New returns a Client for the server at serverURL, an http or https URL
// that may carry a path prefix but no user info, query or fragment. src
// supplies the token of every request attempt. The error for a rejected
// URL never repeats the URL, which may carry a secret.
func New(serverURL string, src TokenSource, opts ...Option) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, errors.New("the Stackorder server URL does not parse as a URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("the Stackorder server URL is not an http or https URL with a host")
	}
	if u.User != nil || strings.ContainsAny(serverURL, "?#") {
		return nil, errors.New("the Stackorder server URL carries user info, a query or a fragment")
	}
	if src == nil {
		return nil, errors.New("no token source for the Stackorder server")
	}
	c := &Client{
		baseURL:   strings.TrimRight(serverURL, "/"),
		tokens:    src,
		http:      withoutRedirects(&http.Client{Timeout: DefaultTimeout}),
		userAgent: userAgent("dev"),
		retry: RetryPolicy{
			Attempts: DefaultAttempts,
			Backoff:  []time.Duration{500 * time.Millisecond, 2 * time.Second},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// StackOutputs calls GET /v1/repos/{owner}/{repo}/stacks/{key}/outputs for
// the stack key in repo, "owner/repo". Inside GitHub Actions, runID and
// reader name the caller's run and its stack, which the server checks
// against the producer's readers; an empty runID or reader is left out of
// the query, as for API keys, which read outside any run.
func (c *Client) StackOutputs(ctx context.Context, repo, stack, runID, reader string) (*v1.StackOutputs, error) {
	if !validRepo(repo) {
		return nil, fmt.Errorf("the repository %q is not owner/repo", repo)
	}
	if stack == "" {
		return nil, errors.New("the stack key is empty")
	}
	if stack == "." || stack == ".." {
		return nil, fmt.Errorf("the stack key %q is not a stack", stack)
	}
	endpoint := c.baseURL + "/v1/repos/" + repo + "/stacks/" + url.PathEscape(stack) + "/outputs"
	query := url.Values{}
	if runID != "" {
		query.Set("run", runID)
	}
	if reader != "" {
		query.Set("reader", reader)
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	qualified := v1.QualifiedStackKey(repo, stack)
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("reading the outputs of %s: %w", qualified, err)
	}
	var out v1.StackOutputs
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("reading the outputs of %s: decoding the response: %w", qualified, err)
	}
	return &out, nil
}

func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		body, err := c.attempt(ctx, endpoint)
		if err == nil {
			return body, nil
		}
		var r retryableError
		if !errors.As(err, &r) || ctx.Err() != nil {
			return nil, err
		}
		if attempt >= c.retry.Attempts {
			return nil, fmt.Errorf("giving up after %d attempts: %w", attempt, err)
		}
		if r.after > maxRetryAfter {
			return nil, fmt.Errorf("not retrying, the answer asks to wait %s: %w", r.after, err)
		}
		timer := time.NewTimer(max(c.retry.wait(attempt), r.after))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting to retry after %w: %w", err, ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) attempt(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("obtaining a token: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, retryable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		apiErr := decodeError(resp)
		if transientStatus(resp.StatusCode) {
			return nil, transient(apiErr, resp.Header)
		}
		return nil, apiErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, retryable(fmt.Errorf("reading the response: %w", err))
	}
	if len(body) > maxResponseBody {
		return nil, fmt.Errorf("the response is larger than %d MiB", maxResponseBody>>20)
	}
	return body, nil
}

func withoutRedirects(hc *http.Client) *http.Client {
	cp := *hc
	cp.CheckRedirect = noRedirects
	return &cp
}

func userAgent(version string) string {
	return "terraform-provider-stackorder/" + version
}

func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && validSegment(owner) && validSegment(name)
}

func validSegment(s string) bool {
	return s != "" && s != "." && s != ".." && url.PathEscape(s) == s
}
