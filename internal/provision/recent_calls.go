package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type RecentCallsQuery struct {
	Page      int    // required
	Count     int    // required
	Days      int    // optional
	Start     string // optional ISO datetime
	End       string // optional ISO datetime
	Direction string // optional "inbound"/"outbound"
	Answered  string // optional "true"/"false"
	Filter    string // optional
}

// ListRecentCalls returns recent calls for the client's own account.
func (c *Client) ListRecentCalls(ctx context.Context, q RecentCallsQuery) (*Paginated, error) {
	if c.accountSID == "" {
		return nil, fmt.Errorf("RecentCalls requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/RecentCalls?" + q.encode()
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/recent_calls/listRecentCalls.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out Paginated
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode RecentCalls: %w", err)
	}
	return &out, nil
}

// ListRecentCallsBySP returns recent calls across all accounts under an SP.
// Requires SP-scoped client.
func (c *Client) ListRecentCallsBySP(ctx context.Context, spSID string, q RecentCallsQuery) (*Paginated, error) {
	path := "/ServiceProviders/" + spSID + "/RecentCalls?" + q.encode()
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/recent_calls/listRecentCallsBySP.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out Paginated
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode RecentCalls: %w", err)
	}
	return &out, nil
}

func (q RecentCallsQuery) encode() string {
	v := url.Values{}
	v.Set("page", strconv.Itoa(q.Page))
	v.Set("count", strconv.Itoa(q.Count))
	if q.Days > 0 {
		v.Set("days", strconv.Itoa(q.Days))
	}
	if q.Start != "" {
		v.Set("start", q.Start)
	}
	if q.End != "" {
		v.Set("end", q.End)
	}
	if q.Direction != "" {
		v.Set("direction", q.Direction)
	}
	if q.Answered != "" {
		v.Set("answered", q.Answered)
	}
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
	return v.Encode()
}
