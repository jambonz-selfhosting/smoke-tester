package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type PhoneNumberCreate struct {
	Number         string `json:"number"`
	VoipCarrierSID string `json:"voip_carrier_sid"`
	AccountSID     string `json:"account_sid,omitempty"`
	ApplicationSID string `json:"application_sid,omitempty"`
}

type PhoneNumber struct {
	PhoneNumberSID string `json:"phone_number_sid"`
	Number         string `json:"number"`
	VoipCarrierSID string `json:"voip_carrier_sid"`
	AccountSID     string `json:"account_sid,omitempty"`
	ApplicationSID string `json:"application_sid,omitempty"`
}

func (c *Client) ProvisionPhoneNumber(ctx context.Context, body PhoneNumberCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/PhoneNumbers", body,
		"rest/phone_numbers/provisionPhoneNumber.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

func (c *Client) GetPhoneNumber(ctx context.Context, sid string) (*PhoneNumber, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/PhoneNumbers/"+sid, nil,
		"rest/phone_numbers/getPhoneNumber.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var p PhoneNumber
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode PhoneNumber: %w", err)
	}
	return &p, nil
}

func (c *Client) ListPhoneNumbers(ctx context.Context) ([]PhoneNumber, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/PhoneNumbers", nil,
		"rest/phone_numbers/listProvisionedPhoneNumbers.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []PhoneNumber
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode phone_numbers: %w", err)
	}
	return out, nil
}

func (c *Client) DeletePhoneNumber(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/PhoneNumbers/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) ManagedPhoneNumber(t *testing.T, ctx context.Context, body PhoneNumberCreate) string {
	t.Helper()
	sid, err := c.ProvisionPhoneNumber(ctx, body)
	if err != nil {
		t.Fatalf("provision phone number: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeletePhoneNumber(cctx, sid); err != nil {
			t.Logf("cleanup: delete phone_number %s: %v", sid, err)
		}
	})
	return sid
}
