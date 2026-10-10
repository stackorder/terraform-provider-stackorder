package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/stackorder/stackorder/api/v1"
)

const maxErrorBody = 1 << 20

// APIError is a final non-200 answer of the server. Code and Message come
// from its v1.Error body; when the body is not one, as from a proxy in front
// of the server, Code is empty and Message holds the redirect target, the
// start of the body or the status text.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the v1.Error code, such as v1.ErrCodeOutputsNotShared.
	Code string
	// Message is the server's reason, written for people.
	Message string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	msg := "the Stackorder server answered " + strconv.Itoa(e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func decodeError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var wire v1.Error
	if err := json.Unmarshal(body, &wire); err == nil && wire.Code != "" {
		return &APIError{Status: resp.StatusCode, Code: wire.Code, Message: wire.Message}
	}
	msg := snippet(body)
	if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		msg = "redirected to " + loc + ", which is never followed"
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{Status: resp.StatusCode, Message: msg}
}

func snippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) <= 200 {
		return s
	}
	n := 200
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func transientStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type retryableError struct {
	err   error
	after time.Duration
}

func (e retryableError) Error() string { return e.err.Error() }

func (e retryableError) Unwrap() error { return e.err }

func retryable(err error) error { return retryableError{err: err} }

func transient(err error, h http.Header) error {
	return retryableError{err: err, after: retryAfter(h, time.Now())}
}

func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if secs, err := strconv.ParseUint(v, 10, 32); err == nil || errors.Is(err, strconv.ErrRange) {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		return max(when.Sub(now), 0)
	}
	return 0
}
