package studio

import (
	"context"
	"net/http"
	"strings"
)

// Situation is one entry of an agent's situations, as the builder saves it.
type Situation struct {
	Name         string   `json:"name"`
	When         string   `json:"when"`
	Instructions string   `json:"instructions"`
	Who          string   `json:"who"` // model | code
	OnResolve    string   `json:"-"`   // told to the agent when the situation ends
	Requires     string   `json:"-"`   // a situation that must be active first; empty for none
	Add          []string `json:"-"`
	Hide         []string `json:"-"`
}

func (s Situation) config() map[string]any {
	add, hide := s.Add, s.Hide
	if add == nil {
		add = []string{}
	}
	if hide == nil {
		hide = []string{}
	}
	var requires any
	if s.Requires != "" {
		requires = s.Requires
	}
	return map[string]any{"name": s.Name, "when": s.When, "instructions": s.Instructions, "who": s.Who,
		"on_resolve": s.OnResolve, "requires": requires, "tools": map[string]any{"add": add, "hide": hide}}
}

// Issue is a compile finding returned with a saved draft.
type Issue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Section  string `json:"section"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

// Pipeline is what CreatePipelineAgent puts in a new agent's draft.
type Pipeline struct {
	PromptID   string
	ToolIDs    []string // HTTP tools, enabled
	Situations []Situation
	Hangup     bool                     // the built-in hangup tool
	Voice      map[string]any           // stt, tts and llm copied from a working agent (PipelineVoice); nil keeps Studio's
	Edit       func(cfg map[string]any) // any other draft change, applied last
}

// PipelineVoice returns the stt, tts and llm settings of an English pipeline agent whose draft has all three
// credentials, and that agent's name: agentID when given, else the first one in the workspace that isn't a smoke
// agent. A new agent starts without credentials, so tests borrow these.
func (c *Client) PipelineVoice(ctx context.Context, agentID string) (map[string]any, string, error) {
	var list struct {
		Agents []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Engine string `json:"engine"`
		} `json:"agents"`
	}
	if err := c.Do(ctx, http.MethodGet, "/agents", nil, &list); err != nil {
		return nil, "", err
	}
	for _, a := range list.Agents {
		if agentID != "" && a.ID != agentID {
			continue
		}
		if (a.Engine != "" && a.Engine != "pipeline") || strings.HasPrefix(a.Name, "smoke ") {
			continue
		}
		v, err := c.agent(ctx, a.ID)
		if err != nil {
			continue
		}
		out, ok := map[string]any{}, true
		for _, k := range []string{"stt", "tts", "llm"} {
			m, _ := v.Draft.Config[k].(map[string]any)
			if m == nil || m["credential_sid"] == nil || m["credential_sid"] == "" {
				ok = false
				break
			}
			out[k] = m
		}
		lang, _ := out["stt"].(map[string]any)["language"].(string)
		if ok && strings.HasPrefix(lang, "en") && out["tts"].(map[string]any)["voice"] != nil && out["llm"].(map[string]any)["model"] != nil {
			return out, a.Name, nil
		}
	}
	return nil, "", nil
}

// CreatePipelineAgent makes a pipeline agent, then saves the voice, prompt, tools and situations into its draft.
// The issues are what that draft compiles to.
func (c *Client) CreatePipelineAgent(ctx context.Context, name string, p Pipeline) (string, []Issue, error) {
	var created struct {
		ID    string `json:"id"`
		Agent *struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	if err := c.Do(ctx, http.MethodPost, "/agents", map[string]any{"name": name, "engine": "pipeline"}, &created); err != nil {
		return "", nil, err
	}
	id := created.ID
	if created.Agent != nil {
		id = created.Agent.ID
	}
	issues, err := c.EditDraft(ctx, id, func(cfg map[string]any) {
		for k, v := range p.Voice {
			cfg[k] = v
		}
		sub(cfg, "prompt")["prompt_id"] = p.PromptID
		tools := []map[string]any{}
		for _, t := range p.ToolIDs {
			tools = append(tools, map[string]any{"tool_id": t, "enabled": true})
		}
		sub(cfg, "tools")["http"] = tools
		if p.Hangup {
			sub(cfg, "hangup")["enabled"] = true
		}
		SetSituations(cfg, p.Situations)
		if p.Edit != nil {
			p.Edit(cfg)
		}
	})
	return id, issues, err
}

// CreateS2sAgentWithSituations makes a realtime voice API agent without a credential and saves situations into it.
func (c *Client) CreateS2sAgentWithSituations(ctx context.Context, name, vendor string, s []Situation) (string, []Issue, error) {
	var created struct {
		Agent *struct {
			ID string `json:"id"`
		} `json:"agent"`
		ID string `json:"id"`
	}
	if err := c.Do(ctx, http.MethodPost, "/agents", map[string]any{"name": name, "engine": "s2s", "s2s_vendor": vendor}, &created); err != nil {
		return "", nil, err
	}
	id := created.ID
	if created.Agent != nil {
		id = created.Agent.ID
	}
	issues, err := c.EditDraft(ctx, id, func(cfg map[string]any) { SetSituations(cfg, s) })
	return id, issues, err
}

// SetSituations replaces the situations in a draft config.
func SetSituations(cfg map[string]any, s []Situation) {
	list := make([]map[string]any, 0, len(s))
	for _, x := range s {
		list = append(list, x.config())
	}
	cfg["situations"] = list
}

// EditDraft loads an agent's draft, applies edit and saves it; it returns the issues the saved draft compiles to.
func (c *Client) EditDraft(ctx context.Context, id string, edit func(cfg map[string]any)) ([]Issue, error) {
	a, err := c.agent(ctx, id)
	if err != nil {
		return nil, err
	}
	edit(a.Draft.Config)
	var out struct {
		Issues []Issue `json:"issues"`
	}
	err = c.Do(ctx, http.MethodPut, "/agents/"+id+"/draft", map[string]any{"revision": a.Draft.Revision, "config": a.Draft.Config}, &out)
	return out.Issues, err
}

// Verb returns the verbs a test call on the agent's draft starts with.
func (c *Client) Verb(ctx context.Context, id string) ([]map[string]any, error) {
	var out struct {
		Ack []map[string]any `json:"ack"`
	}
	err := c.Do(ctx, http.MethodGet, "/agents/"+id+"/verb", nil, &out)
	return out.Ack, err
}

// CreateTool adds an HTTP tool and returns its id.
func (c *Client) CreateTool(ctx context.Context, def map[string]any) (string, error) {
	var out struct {
		ID   string `json:"id"`
		Tool *struct {
			ID string `json:"id"`
		} `json:"tool"`
	}
	if err := c.Do(ctx, http.MethodPost, "/tools", def, &out); err != nil {
		return "", err
	}
	if out.Tool != nil {
		return out.Tool.ID, nil
	}
	return out.ID, nil
}

// HTTPGetTool is a GET tool whose parameters go in the query string and whose full JSON response reaches the agent.
func HTTPGetTool(name, description, url string, params map[string]string) map[string]any {
	props := map[string]any{}
	required := []string{}
	for k, d := range params {
		props[k] = map[string]any{"type": "string", "description": d}
		required = append(required, k)
	}
	return map[string]any{
		"name": name, "description": description, "method": "GET", "url": url,
		"headers": []any{}, "auth": map[string]any{"kind": "none"}, "send_as": "query",
		"parameters": map[string]any{"type": "object", "properties": props, "required": required},
		"response":   map[string]any{"mode": "full", "max_chars": 2000},
		"timeout_ms": 8000, "retries": 0,
	}
}

func sub(cfg map[string]any, k string) map[string]any {
	m, _ := cfg[k].(map[string]any)
	if m == nil {
		m = map[string]any{}
		cfg[k] = m
	}
	return m
}

// Sub returns cfg[k] as a map, creating it when absent.
func Sub(cfg map[string]any, k string) map[string]any { return sub(cfg, k) }

// Publish makes the agent's current draft its live version.
func (c *Client) Publish(ctx context.Context, id string) error {
	a, err := c.agent(ctx, id)
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodPost, "/agents/"+id+"/publish", map[string]any{"revision": a.Draft.Revision}, nil)
}

// AssignNumber routes a jambonz phone number to the agent; UnassignNumber undoes it.
func (c *Client) AssignNumber(ctx context.Context, id, numberSID string) error {
	return c.Do(ctx, http.MethodPut, "/agents/"+id+"/numbers", map[string]any{"phone_number_sid": numberSID, "replace_application": true}, nil)
}

func (c *Client) UnassignNumber(ctx context.Context, id, numberSID string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/agents/"+id+"/numbers/"+numberSID, nil, nil))
}

// CreateMcpServer adds an MCP server that allows all its tools and returns its id.
func (c *Client) CreateMcpServer(ctx context.Context, name, url string) (string, error) {
	var out struct {
		ID     string `json:"id"`
		Server *struct {
			ID string `json:"id"`
		} `json:"server"`
	}
	body := map[string]any{"name": name, "url": url, "transport": "streamable_http", "allow_new_tools": true}
	if err := c.Do(ctx, http.MethodPost, "/mcp-servers", body, &out); err != nil {
		return "", err
	}
	if out.Server != nil {
		return out.Server.ID, nil
	}
	return out.ID, nil
}

func (c *Client) DeleteMcpServer(ctx context.Context, id string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/mcp-servers/"+id, nil, nil))
}

// CallEvent is one entry of a call's timeline.
type CallEvent struct {
	Text      string `json:"text"`
	Sub       string `json:"sub"`
	Situation *struct {
		Name   string `json:"name"`
		Change string `json:"change"`
	} `json:"situation"`
}

// CallEvents returns the timeline Studio shows for a call.
func (c *Client) CallEvents(ctx context.Context, callSID string) ([]CallEvent, error) {
	var out struct {
		Events []CallEvent `json:"events"`
	}
	err := c.Do(ctx, http.MethodGet, "/calls/"+callSID, nil, &out)
	return out.Events, err
}

// CallTurn is one turn of a call as Studio records it.
type CallTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// CallTurns returns the call's turns in order.
func (c *Client) CallTurns(ctx context.Context, callSID string) ([]CallTurn, error) {
	var out struct {
		Turns []CallTurn `json:"turns"`
	}
	err := c.Do(ctx, http.MethodGet, "/calls/"+callSID, nil, &out)
	return out.Turns, err
}
