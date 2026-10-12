// Tests for the `llm` verb with LLM vendor "elevenlabs" (ElevenLabs Agents,
// realtime S2S) — caller DTMF forwarded to the agent.
//
// ElevenLabs only takes keypad input natively on its own telephony paths. On
// the agent WebSocket the client must send it as a user_message with
// source_medium "dtmf", and every user_message is a separate user turn — the
// agent's dtmf_input_settings (digit timeout, # terminator) do NOT apply. So
// the verb buffers digits (llmOptions.dtmf) and sends the whole entry as one
// message; sending per digit makes "1234" four turns and the agent loses it.
//
// The test provisions its own agent through the ElevenLabs API (deleted on
// cleanup), so only ELEVENLABS_API_KEY is needed. Unset = pass without
// exercising ElevenLabs — a plain `return` after a log, matching
// google_llm_s2s_test.go.
package verbs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	elevenlabsAPIBase   = "https://api.elevenlabs.io"
	elevenlabsEventHook = "action/llm-elevenlabs-event"

	// Constrains every reply to a checkable pattern: the number back, digit by digit.
	elevenlabsDTMFPrompt = `You are a phone agent that collects numbers. ` +
		`Whenever the user gives you a number, by keypad or by text, reply with exactly ` +
		`"You entered" followed by that number read one digit at a time. Say nothing else.`
)

// TestVerb_LLM_ElevenLabs_DTMF proves caller DTMF reaches an ElevenLabs agent
// as whole entries, both ways the verb ends an entry, and that llm:update
// forwards a user_message.
//
// Steps:
//  1. create-agent — ElevenLabs agent with dtmf_input_settings, deleted on cleanup
//  2. script-and-call — llm vendor elevenlabs + llmOptions.dtmf + eventHook, WS transport
//  3. answer-and-silence
//  4. wait-for-greeting — conversation_initiation_metadata + first agent_response
//  5. dtmf-finish-on-key — send "1234#"; one user turn "1234", reply reads 1234
//  6. dtmf-inter-digit-timeout — send "567", no "#"; one user turn "567" after the timeout
//  7. llm-update-user-message — llm:update user_message; reply reads 2468
//  8. hangup-and-wait-ended
//  9. assert-vendor-transcript — ElevenLabs' stored transcript has exactly the
//     dtmf user turns ["1234", "567"]
//  10. assert-contract — every eventHook payload matches its schema
func TestVerb_LLM_ElevenLabs_DTMF(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasElevenlabs() {
		t.Log("ELEVENLABS_API_KEY not set — passing without exercising ElevenLabs S2S DTMF")
		return
	}

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "create-agent")
	agentID, err := elevenlabsCreateAgent(ctx, provision.Name("dtmf"))
	if err != nil {
		s.Fatalf("create ElevenLabs agent: %v", err)
	}
	t.Cleanup(func() {
		if err := elevenlabsAPI(context.Background(), http.MethodDelete, "/v1/convai/agents/"+agentID, nil, nil); err != nil {
			t.Logf("delete ElevenLabs agent %s: %v", agentID, err)
		}
	})
	s.Logf("agent_id=%s", agentID)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-and-call")
	llmVerb := V("llm",
		"vendor", "elevenlabs",
		"auth", map[string]any{"agent_id": agentID, "api_key": cfg.ElevenlabsAPIKey},
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-elevenlabs-event"),
		"llmOptions", map[string]any{
			"dtmf": map[string]any{"enable": true, "interDigitTimeout": 2, "finishOnKey": "#"},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm", "llm-elevenlabs-event")
	// WS transport so llm:update can be sent as a session command.
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	var drained []webhook.Callback

	s = Step(t, "wait-for-greeting")
	turn := elevenlabsWaitTurn(sess, 20*time.Second, false)
	drained = append(drained, turn.events...)
	convID := ""
	for _, cb := range elevenlabsEvents(turn.events, "conversation_initiation_metadata") {
		convID = cb.NestedString("conversation_initiation_metadata_event.conversation_id")
	}
	if convID == "" {
		s.Fatalf("no conversation_initiation_metadata within 20s; events seen: %v", elevenlabsTypes(turn.events))
	}
	if turn.reply == "" {
		s.Fatalf("agent never greeted; events seen: %v", elevenlabsTypes(turn.events))
	}
	s.Logf("conversation_id=%s greeting=%q", convID, turn.reply)
	s.Done()

	s = Step(t, "dtmf-finish-on-key")
	if err := call.SendDTMF("1234#"); err != nil {
		s.Fatalf("SendDTMF: %v", err)
	}
	turn = elevenlabsWaitTurn(sess, 20*time.Second, true)
	drained = append(drained, turn.events...)
	elevenlabsAssertTurn(s, turn, "1234")
	s.Done()

	s = Step(t, "dtmf-inter-digit-timeout")
	if err := call.SendDTMF("567"); err != nil {
		s.Fatalf("SendDTMF: %v", err)
	}
	turn = elevenlabsWaitTurn(sess, 20*time.Second, true)
	drained = append(drained, turn.events...)
	elevenlabsAssertTurn(s, turn, "567")
	s.Done()

	s = Step(t, "llm-update-user-message")
	const updateText = "The number is 2468."
	if err := sess.SendCommand("llm:update", map[string]any{
		"type": "user_message",
		"text": updateText,
	}); err != nil {
		s.Fatalf("SendCommand(llm:update user_message): %v", err)
	}
	// ElevenLabs emits no user_transcript for a text user_message, so the reply is the only proof.
	turn = elevenlabsWaitTurn(sess, 20*time.Second, false)
	drained = append(drained, turn.events...)
	if got := digitsSpoken(turn.reply); !strings.Contains(got, "2468") {
		s.Errorf("agent reply %q (digits %q) does not read back 2468", turn.reply, got)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
	drained = append(drained, DrainCallbacks(sess, 3*time.Second)...)

	s = Step(t, "assert-vendor-transcript")
	// ElevenLabs' own record is the authority on how many turns it created.
	dtmfTurns, err := elevenlabsDTMFTurns(ctx, convID)
	if err != nil {
		s.Fatalf("read ElevenLabs conversation %s: %v", convID, err)
	}
	if !slices.Equal(dtmfTurns, []string{"1234", "567"}) {
		s.Errorf("ElevenLabs recorded dtmf user turns %q, want [\"1234\" \"567\"]", dtmfTurns)
	}
	s.Done()

	s = Step(t, "assert-contract")
	n := 0
	for _, cb := range drained {
		if cb.Hook != elevenlabsEventHook {
			continue
		}
		n++
		if err := webhookSrv.Validator.ValidateResponse("callbacks/llm-elevenlabs-event.schema.json", cb.Body); err != nil {
			s.Errorf("%s payload violates its contract: %v; body=%s", cb.Hook, err, cb.Body)
		}
	}
	if n == 0 {
		s.Errorf("no elevenlabs eventHook callbacks to validate")
	}
	s.Logf("validated %d elevenlabs callbacks", n)
	s.Done()
}

// elevenlabsTurn is one exchange read off the eventHook stream.
type elevenlabsTurn struct {
	events    []webhook.Callback
	userTurns []string // user_transcript texts, in order
	reply     string   // first substantive agent_response after them
}

// elevenlabsWaitTurn drains eventHook traffic until the agent answers. With
// needUser, only a reply that follows a user turn counts; the greeting doesn't.
// A trailing grace drain catches a late extra user turn, the failure we test for.
func elevenlabsWaitTurn(sess *webhook.Session, within time.Duration, needUser bool) elevenlabsTurn {
	var turn elevenlabsTurn
	for deadline := time.Now().Add(within); turn.reply == "" && time.Now().Before(deadline); {
		batch := DrainCallbacks(sess, 500*time.Millisecond)
		turn.events = append(turn.events, batch...)
		for _, cb := range batch {
			if cb.Hook != elevenlabsEventHook || turn.reply != "" {
				continue
			}
			switch cb.String("type") {
			case "user_transcript":
				turn.userTurns = append(turn.userTurns, cb.NestedString("user_transcription_event.user_transcript"))
			case "agent_response":
				// "..." is the placeholder ElevenLabs emits for an interrupted response
				r := cb.NestedString("agent_response_event.agent_response")
				if strings.Trim(r, ". ") != "" && (!needUser || len(turn.userTurns) > 0) {
					turn.reply = r
				}
			}
		}
	}
	grace := DrainCallbacks(sess, 3*time.Second)
	turn.events = append(turn.events, grace...)
	for _, cb := range elevenlabsEvents(grace, "user_transcript") {
		turn.userTurns = append(turn.userTurns, cb.NestedString("user_transcription_event.user_transcript"))
	}
	return turn
}

// elevenlabsAssertTurn checks the digits arrived as ONE user turn and the
// agent read them back, which proves it received the entry whole.
func elevenlabsAssertTurn(s *StepCtx, turn elevenlabsTurn, digits string) {
	if !slices.Equal(turn.userTurns, []string{digits}) {
		s.Errorf("user turns=%q want exactly [%q] — digits split across turns or lost", turn.userTurns, digits)
	}
	if turn.reply == "" {
		s.Errorf("no agent reply after the %q entry; events seen: %v", digits, elevenlabsTypes(turn.events))
		return
	}
	if got := digitsSpoken(turn.reply); !strings.Contains(got, digits) {
		s.Errorf("agent reply %q (digits %q) does not read back %s", turn.reply, got, digits)
	}
	s.Logf("user turns=%q reply=%q", turn.userTurns, turn.reply)
}

func elevenlabsEvents(cbs []webhook.Callback, ty string) []webhook.Callback {
	var out []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == elevenlabsEventHook && cb.String("type") == ty {
			out = append(out, cb)
		}
	}
	return out
}

func elevenlabsTypes(cbs []webhook.Callback) []string {
	var out []string
	for _, cb := range cbs {
		if cb.Hook == elevenlabsEventHook {
			out = append(out, cb.String("type"))
		}
	}
	return out
}

// digitsSpoken extracts the digit sequence from a reply, whether the model
// wrote "1234" or "one two three four".
func digitsSpoken(s string) string {
	words := map[string]string{
		"zero": "0", "oh": "0", "one": "1", "two": "2", "three": "3", "four": "4",
		"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	}
	var b strings.Builder
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if d, ok := words[w]; ok {
			b.WriteString(d)
			continue
		}
		for _, r := range w {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// --- ElevenLabs REST -------------------------------------------------------

func elevenlabsAPI(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, elevenlabsAPIBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("xi-api-key", cfg.ElevenlabsAPIKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func elevenlabsCreateAgent(ctx context.Context, name string) (string, error) {
	var res struct {
		AgentID string `json:"agent_id"`
	}
	err := elevenlabsAPI(ctx, http.MethodPost, "/v1/convai/agents/create", map[string]any{
		"name": name,
		"conversation_config": map[string]any{
			"agent": map[string]any{
				"first_message": "Please enter your number.",
				"language":      "en",
				"prompt":        map[string]any{"prompt": elevenlabsDTMFPrompt},
			},
			"conversation": map[string]any{
				"dtmf_input_settings": map[string]any{"dtmf_input_timeout": 2, "hash_terminator": true},
			},
		},
	}, &res)
	if err == nil && res.AgentID == "" {
		err = fmt.Errorf("no agent_id in response")
	}
	return res.AgentID, err
}

// elevenlabsDTMFTurns returns the user turns ElevenLabs stored with
// source_medium "dtmf", polling until the conversation has been processed.
func elevenlabsDTMFTurns(ctx context.Context, convID string) ([]string, error) {
	var conv struct {
		Status     string `json:"status"`
		Transcript []struct {
			Role         string `json:"role"`
			Message      string `json:"message"`
			SourceMedium string `json:"source_medium"`
		} `json:"transcript"`
	}
	for deadline := time.Now().Add(30 * time.Second); ; {
		if err := elevenlabsAPI(ctx, http.MethodGet, "/v1/convai/conversations/"+convID, nil, &conv); err != nil {
			return nil, err
		}
		if conv.Status == "done" || conv.Status == "failed" || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	var out []string
	for _, m := range conv.Transcript {
		if m.Role == "user" && m.SourceMedium == "dtmf" {
			out = append(out, m.Message)
		}
	}
	return out, nil
}
