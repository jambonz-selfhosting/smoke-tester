package studio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// OrderStatusTool is the HTTP tool the order-desk scenarios expect the agent to call.
var OrderStatusTool = map[string]any{
	"type":        "http",
	"name":        "get_order_status",
	"description": "Look up an order by its id. Returns the order status in the status field.",
	"method":      "GET",
	"url":         "https://postman-echo.com/get?status=shipped%20yesterday",
	"headers":     []any{},
	"auth":        map[string]any{"kind": "none"},
	"send_as":     "query",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id": map[string]any{"type": "string", "description": "The order number the caller gives, digits only."},
		},
		"required": []string{"order_id"},
	},
	"response":   map[string]any{"mode": "full", "max_chars": 2000},
	"timeout_ms": 8000,
	"retries":    0,
}

// OrderDeskPrompt is the order-desk brief every vendor's agent runs (scenarios/studio-s2s/README.md).
const OrderDeskPrompt = `You are the order-status desk for Brightway Home, speaking on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help.
When the caller asks about an order, call get_order_status with the order number, then tell them the status it returns.
When the caller asks about billing, tell them you are putting them through to billing and use the transfer tool. If the transfer does not go through, say billing is not available right now and ask if there is anything else.
When the caller says they are done or says goodbye, say goodbye and end the call with the hangup tool.`

// BillingNumber never answers, so the transfer comes back to the agent.
const BillingNumber = "+15005550006"

// EnsureTool returns the workspace's tool with def's name, creating it when there is none.
func (c *Client) EnsureTool(ctx context.Context, def map[string]any) (id string, created bool, err error) {
	var tools []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.Do(ctx, http.MethodGet, "/tools", nil, &tools); err != nil {
		return "", false, err
	}
	for _, t := range tools {
		if t.Name == def["name"] {
			return t.ID, false, nil
		}
	}
	var out struct {
		ID   string `json:"id"`
		Tool *struct {
			ID string `json:"id"`
		} `json:"tool"`
	}
	if err := c.Do(ctx, http.MethodPost, "/tools", def, &out); err != nil {
		return "", false, err
	}
	if out.Tool != nil {
		return out.Tool.ID, true, nil
	}
	return out.ID, true, nil
}

func (c *Client) CreatePrompt(ctx context.Context, name, text string) (string, error) {
	body := map[string]any{"name": name, "kind": "prompt",
		"body": map[string]any{"mode": "text", "text": text, "append_builtin": true, "guardrail_ids": []string{}}}
	var out struct {
		ID     string `json:"id"`
		Prompt *struct {
			ID string `json:"id"`
		} `json:"prompt"`
	}
	if err := c.Do(ctx, http.MethodPost, "/prompts", body, &out); err != nil {
		return "", err
	}
	if out.Prompt != nil {
		return out.Prompt.ID, nil
	}
	return out.ID, nil
}

// S2s is an agent's realtime voice API settings; Model and Voice empty keep Studio's starting values.
type S2s struct {
	Vendor        string // credential vendor, e.g. xai_s2s
	CredentialSID string
	Label         string // the credential's label; empty for an unlabelled one
	Model, Voice  string
}

// OrderDesk is what CreateS2sAgent puts in the draft besides the s2s settings.
type OrderDesk struct {
	PromptID, ToolID string
}

type agentView struct {
	ID    string `json:"id"`
	Draft struct {
		Revision int            `json:"revision"`
		Config   map[string]any `json:"config"`
	} `json:"draft"`
}

func (c *Client) agent(ctx context.Context, id string) (*agentView, error) {
	var raw struct {
		agentView
		Agent *agentView `json:"agent"`
	}
	if err := c.Do(ctx, http.MethodGet, "/agents/"+id, nil, &raw); err != nil {
		return nil, err
	}
	if raw.Agent != nil {
		return raw.Agent, nil
	}
	return &raw.agentView, nil
}

// CreateS2sAgent makes a realtime voice API agent running the order desk: the prompt, the order tool, a
// billing transfer that returns, and the hangup tool.
func (c *Client) CreateS2sAgent(ctx context.Context, name string, s S2s, d OrderDesk) (string, error) {
	var created struct {
		ID    string `json:"id"`
		Agent *struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	body := map[string]any{"name": name, "engine": "s2s", "s2s_vendor": s.Vendor}
	if err := c.Do(ctx, http.MethodPost, "/agents", body, &created); err != nil {
		return "", err
	}
	id := created.ID
	if created.Agent != nil {
		id = created.Agent.ID
	}
	a, err := c.agent(ctx, id)
	if err != nil {
		return id, err
	}
	cfg := a.Draft.Config
	sub := func(k string) map[string]any {
		m, _ := cfg[k].(map[string]any)
		if m == nil {
			m = map[string]any{}
			cfg[k] = m
		}
		return m
	}
	sub("prompt")["prompt_id"] = d.PromptID
	sub("tools")["http"] = []map[string]any{{"tool_id": d.ToolID, "enabled": true}}
	tr := sub("transfer")
	tr["enabled"] = true
	tr["destination"] = map[string]any{"type": "phone", "number": BillingNumber}
	tr["description"] = "Billing questions go to the billing team."
	tr["mode"] = "blind-dial"
	tr["timeout_s"] = 10
	ss := sub("s2s")
	ss["vendor"] = s.Vendor
	ss["credential_sid"] = s.CredentialSID
	if s.Label == "" {
		ss["label"] = nil
	} else {
		ss["label"] = s.Label
	}
	if s.Model != "" {
		ss["model"] = s.Model
	}
	if s.Voice != "" {
		ss["voice"] = s.Voice
	}
	err = c.Do(ctx, http.MethodPut, "/agents/"+id+"/draft", map[string]any{"revision": a.Draft.Revision, "config": cfg}, nil)
	return id, err
}

// DeleteAgent removes an agent; one already gone is not an error.
func (c *Client) DeleteAgent(ctx context.Context, id, name string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/agents/"+id, map[string]string{"confirm_name": name}, nil))
}

func (c *Client) DeletePrompt(ctx context.Context, id string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/prompts/"+id, nil, nil))
}

func (c *Client) DeleteTool(ctx context.Context, id string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/tools/"+id, nil, nil))
}

func ignoreNotFound(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	return nil
}
