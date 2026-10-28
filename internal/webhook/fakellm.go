package webhook

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Fake OpenAI-compatible chat-completions server, mounted at
// /llm/<test-id>/v1. An agent verb pointed at it (vendor "openai",
// auth.baseURL = that URL) lets a test script each LLM response: normal
// text, tool calls, HTTP errors, a hang, or a stream cut mid-reply.
// Failure modes a real vendor cannot be made to produce on demand.

// LLMReply scripts one chat-completions response. The zero value echoes the
// caller's last user message.
type LLMReply struct {
	Status        int    // non-2xx: an OpenAI-shaped error with this status
	ErrorCode     string // error.code for a Status reply, e.g. "insufficient_quota"
	RetryAfter    string // Retry-After header for a Status reply
	Text          string
	ToolCalls     []LLMToolCall
	Hang          bool // send nothing until the client gives up
	DropAfterText bool // stream Text, then cut the connection without finishing
}

// LLMToolCall is one function call in a scripted reply. Args is a JSON string.
type LLMToolCall struct {
	ID, Name, Args string
}

// LLMRequest is one chat-completions request the fake received.
type LLMRequest struct {
	Received time.Time
	Body     map[string]any
}

type fakeLLM struct {
	mu       sync.Mutex
	replies  []LLMReply
	requests []LLMRequest
}

// ScriptLLM queues replies for this session's fake LLM, served in order.
// Requests beyond the queue get the echo reply.
func (s *Session) ScriptLLM(replies ...LLMReply) *Session {
	s.llm.mu.Lock()
	defer s.llm.mu.Unlock()
	s.llm.replies = append(s.llm.replies, replies...)
	return s
}

// LLMRequests returns every chat-completions request received so far.
func (s *Session) LLMRequests() []LLMRequest {
	s.llm.mu.Lock()
	defer s.llm.mu.Unlock()
	return append([]LLMRequest(nil), s.llm.requests...)
}

func (s *Session) nextLLMReply(body map[string]any) LLMReply {
	s.llm.mu.Lock()
	defer s.llm.mu.Unlock()
	s.llm.requests = append(s.llm.requests, LLMRequest{Received: time.Now(), Body: body})
	if len(s.llm.replies) == 0 {
		return LLMReply{Text: lastUserText(body)}
	}
	r := s.llm.replies[0]
	s.llm.replies = s.llm.replies[1:]
	return r
}

func lastUserText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] == "user" {
			if c, ok := m["content"].(string); ok && c != "" {
				return c
			}
		}
	}
	return "Okay."
}

// handleFakeLLM serves /llm/<test-id>/v1/{chat/completions,models}.
func (s *Server) handleFakeLLM(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/llm/")
	testID, path, ok := strings.Cut(rest, "/v1/")
	if !ok {
		http.Error(w, "want /llm/<test-id>/v1/...", http.StatusNotFound)
		return
	}
	sess, found := s.Registry.Lookup(testID)
	if !found {
		http.Error(w, "unknown test id", http.StatusNotFound)
		return
	}
	switch path {
	case "models":
		// warmup lists models; it must not consume a scripted reply
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		return
	case "chat/completions":
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body map[string]any
	_ = json.Unmarshal(readAll(r), &body)
	reply := sess.nextLLMReply(body)
	model, _ := body["model"].(string)

	switch {
	case reply.Hang:
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute):
		}
		return
	case reply.Status >= 300:
		if reply.RetryAfter != "" {
			w.Header().Set("Retry-After", reply.RetryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "fake llm: scripted " + http.StatusText(reply.Status),
			"type":    "fake_error",
			"code":    reply.ErrorCode,
		}})
		return
	}

	if stream, _ := body["stream"].(bool); !stream {
		writeCompletion(w, model, reply)
		return
	}
	streamCompletion(w, model, reply, wantsUsage(body))
}

func wantsUsage(body map[string]any) bool {
	opts, _ := body["stream_options"].(map[string]any)
	v, _ := opts["include_usage"].(bool)
	return v
}

func toolCallsJSON(calls []LLMToolCall, indexed bool) []any {
	out := make([]any, 0, len(calls))
	for i, tc := range calls {
		c := map[string]any{
			"id":       tc.ID,
			"type":     "function",
			"function": map[string]any{"name": tc.Name, "arguments": tc.Args},
		}
		if indexed {
			c["index"] = i
		}
		out = append(out, c)
	}
	return out
}

func finishReason(reply LLMReply) string {
	if len(reply.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func writeCompletion(w http.ResponseWriter, model string, reply LLMReply) {
	msg := map[string]any{"role": "assistant", "content": reply.Text}
	if len(reply.ToolCalls) > 0 {
		msg["tool_calls"] = toolCallsJSON(reply.ToolCalls, false)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finishReason(reply)}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
}

func streamCompletion(w http.ResponseWriter, model string, reply LLMReply, usage bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}

	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if reply.Text != "" {
		send(chunk(map[string]any{"content": reply.Text}, nil))
	}
	if reply.DropAfterText {
		// let the text reach the client, then kill the connection mid-stream
		time.Sleep(300 * time.Millisecond)
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	if len(reply.ToolCalls) > 0 {
		send(chunk(map[string]any{"tool_calls": toolCallsJSON(reply.ToolCalls, true)}, nil))
	}
	send(chunk(map[string]any{}, finishReason(reply)))
	if usage {
		send(map[string]any{
			"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []any{},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}
