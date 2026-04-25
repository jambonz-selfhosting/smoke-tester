package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

type AlertsQuery struct {
	Page      int
	Count     int
	Days      int
	Start     string
	End       string
	AlertType string
}

func (c *Client) ListAlerts(ctx context.Context, q AlertsQuery) (*Paginated, error) {
	if c.accountSID == "" {
		return nil, fmt.Errorf("Alerts requires an account-scoped client")
	}
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
	if q.AlertType != "" {
		v.Set("alert_type", q.AlertType)
	}
	path := "/Accounts/" + c.accountSID + "/Alerts?" + v.Encode()
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/alerts/listAlertsByAccount.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out Paginated
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode Alerts: %w", err)
	}
	return &out, nil
}
