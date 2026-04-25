package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ListRegisteredSipUsers returns the list of currently-registered SIP users
// for the account (empty on a quiet cluster).
func (c *Client) ListRegisteredSipUsers(ctx context.Context) ([]string, error) {
	if c.accountSID == "" {
		return nil, fmt.Errorf("RegisteredSipUsers requires an account-scoped client")
	}
	raw, err := c.Request(ctx, http.MethodGet, "/Accounts/"+c.accountSID+"/RegisteredSipUsers", nil,
		"rest/registered_sip_users/listRegisteredSipUsers.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode RegisteredSipUsers: %w", err)
	}
	return out, nil
}
