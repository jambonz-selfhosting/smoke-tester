package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// GetWebhookSecret returns the current signing secret for an account. If
// regenerate=true, the server rotates the secret first.
func (c *Client) GetWebhookSecret(ctx context.Context, regenerate bool) (string, error) {
	if c.accountSID == "" {
		return "", fmt.Errorf("WebhookSecret requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/WebhookSecret"
	if regenerate {
		path += "?regenerate=true"
	}
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/webhook_secret/getWebhookSecret.response.200.json", http.StatusOK)
	if err != nil {
		return "", err
	}
	var out struct {
		WebhookSecret string `json:"webhook_secret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode WebhookSecret: %w", err)
	}
	return out.WebhookSecret, nil
}
