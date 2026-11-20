package studio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookie = "__Host-studio_sid"

// Client calls the Studio API as a signed-in user, scoped to one workspace.
type Client struct {
	apiBase   string // e.g. https://studio.jambonz.io/v1
	origin    string // Studio refuses writes whose Origin is not its own host
	sid       string
	AccountID string
	hc        *http.Client
}

func New(apiBase, sid string) (*Client, error) {
	u, err := url.Parse(apiBase)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("studio API URL %q is not a URL", apiBase)
	}
	return &Client{
		apiBase: strings.TrimRight(apiBase, "/"),
		origin:  u.Scheme + "://" + u.Host,
		sid:     strings.TrimSpace(sid),
		hc:      &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// SignIn returns a session cookie value. Studio rate-limits sign-in, so a run signs in once.
func SignIn(ctx context.Context, apiBase, email, password string) (string, error) {
	c, err := New(apiBase, "")
	if err != nil {
		return "", err
	}
	res, err := c.send(ctx, http.MethodPost, c.apiBase+"/auth/sign-in", map[string]string{"email": email, "password": password})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return "", fmt.Errorf("studio sign-in: %d %s", res.StatusCode, body)
	}
	for _, ck := range res.Cookies() {
		if ck.Name == sessionCookie {
			return ck.Value, nil
		}
	}
	return "", errors.New("studio sign-in: no session cookie in the response")
}

func (c *Client) send(ctx context.Context, method, full string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rd)
	if err != nil {
		return nil, err
	}
	if c.sid != "" {
		req.Header.Set("Cookie", sessionCookie+"="+c.sid)
	}
	req.Header.Set("Origin", c.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.hc.Do(req)
}

func (c *Client) call(ctx context.Context, method, full string, body, out any) error {
	res, err := c.send(ctx, method, full, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return &APIError{Method: method, Path: strings.TrimPrefix(full, c.apiBase), Status: res.StatusCode, Body: string(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Do calls a workspace route: path is relative to /accounts/<AccountID>.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	return c.call(ctx, method, fmt.Sprintf("%s/accounts/%s%s", c.apiBase, c.AccountID, path), body, out)
}

type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("studio %s %s: %d %s", e.Method, e.Path, e.Status, e.Body)
}

// UseWorkspace sets AccountID: id when given, else the login's first workspace it owns or administers.
func (c *Client) UseWorkspace(ctx context.Context, id string) error {
	if id != "" {
		c.AccountID = id
		return nil
	}
	var me struct {
		Accounts []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"accounts"`
	}
	if err := c.call(ctx, http.MethodGet, c.apiBase+"/me", nil, &me); err != nil {
		return err
	}
	for _, a := range me.Accounts {
		if a.Role == "owner" || a.Role == "admin" {
			c.AccountID = a.ID
			return nil
		}
	}
	return errors.New("studio: this login owns or administers no workspace")
}

// Connection is the jambonz account the workspace runs its agents on.
type Connection struct {
	APIBaseURL        string `json:"api_base_url"`
	JambonzAccountSID string `json:"jambonz_account_sid"`
	SIPRealm          string `json:"sip_realm"`
	Hostname          string `json:"hostname"`
	Status            string `json:"status"`
}

func (c *Client) Connection(ctx context.Context) (*Connection, error) {
	var conn Connection
	if err := c.Do(ctx, http.MethodGet, "/connection", nil, &conn); err != nil {
		return nil, err
	}
	return &conn, nil
}
