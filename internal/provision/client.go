// Package provision is the typed Go client for the jambonz REST API used by
// the test harness. See ADR-0008 (run-id/cleanup) and ADR-0015 (contract).
package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
)

// Client wraps http.Client with auth, base URL, and contract validation.
type Client struct {
	base       string
	apiKey     string
	hc         *http.Client
	validator  *contract.Validator
	accountSID string
	label      string // optional tag for logs (e.g. "account" or "sp")
}

type Option func(*Client)

func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }
func WithLabel(label string) Option         { return func(c *Client) { c.label = label } }

// New constructs a Client pointed at baseURL (e.g. https://jambonz.me/api/v1).
// The validator is required — per ADR-0015, every response is contract-checked.
// accountSID may be empty for SP-scope clients.
func New(baseURL, apiKey, accountSID string, v *contract.Validator, opts ...Option) *Client {
	c := &Client{
		base:       baseURL,
		apiKey:     apiKey,
		accountSID: accountSID,
		hc:         &http.Client{Timeout: 20 * time.Second},
		validator:  v,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// AccountSID is exposed so callers can build nested paths.
func (c *Client) AccountSID() string { return c.accountSID }

// Label returns the human-readable tag for logs.
func (c *Client) Label() string { return c.label }

// APIError describes a non-2xx response in a structured way.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   []byte
	Msg    string // from jambonz GeneralError.msg if parseable
}

func (e *APIError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s %s -> %d: %s", e.Method, e.Path, e.Status, e.Msg)
	}
	return fmt.Sprintf("%s %s -> %d: %s", e.Method, e.Path, e.Status, truncate(string(e.Body), 200))
}

// AsAPIError unwraps err to *APIError and returns (apiErr, true) if it is
// one. The status code can be read from apiErr.Status. Use in tests:
//
//	if apiErr, ok := provision.AsAPIError(err); ok {
//	    require.Equal(t, http.StatusNotFound, apiErr.Status)
//	}
//
// Returns (nil, false) on any other error type, including success.
func AsAPIError(err error) (*APIError, bool) {
	if err == nil {
		return nil, false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// StatusOf is the one-liner you usually want: returns the HTTP status from
// err if it's an *APIError, or 0 otherwise. Pairs with require.Equal in
// callers without the errors.As ceremony.
func StatusOf(err error) int {
	if apiErr, ok := AsAPIError(err); ok {
		return apiErr.Status
	}
	return 0
}

// Request performs an HTTP call and returns the response bytes.
//
//   - Sets Authorization: Bearer and Content-Type when body != nil.
//   - When schemaRelPath is non-empty and status matches, validates the
//     response body against schemas/<schemaRelPath>. Per ADR-0015, missing
//     schema files surface as contract.ErrNoSchema (must be fixed, not
//     silenced).
//   - Non-2xx: attempts to parse jambonz GeneralError and returns *APIError.
func (c *Client) Request(ctx context.Context, method, path string, body any, schemaRelPath string, expectStatus int) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(blob)
	}
	url := c.base + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, path, err)
	}

	if resp.StatusCode != expectStatus {
		apiErr := &APIError{
			Method: method,
			Path:   path,
			Status: resp.StatusCode,
			Body:   respBody,
		}
		var ge struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(respBody, &ge) == nil {
			apiErr.Msg = ge.Msg
		}
		return respBody, apiErr
	}

	if schemaRelPath != "" && len(respBody) > 0 {
		if err := c.validator.ValidateResponse(schemaRelPath, respBody); err != nil {
			if errors.Is(err, contract.ErrNoSchema) {
				return respBody, err
			}
			return respBody, err
		}
	}

	return respBody, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
