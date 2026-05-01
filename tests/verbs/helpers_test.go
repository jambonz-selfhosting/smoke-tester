// Shared helpers for the verbs package test suite. Every test file in this
// package is free to use anything declared here — no need to redefine local
// helpers. Keep this file focused on test-only concerns; anything useful to
// non-test code belongs on the smoke-tester package APIs it's built from.
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// --- per-test UAS lifecycle -----------------------------------------------
//
// Background: the suite used to share a single UAS (the JAMBONZ_SIP_USER
// stack) plus an optional callee UAS — both routed inbound INVITEs through
// package-level singleton channels. That forced every test to run serially:
// two tests in flight would race for the same channel.
//
// claimUAS replaces both. Each test provisions its own SIP user via the
// /Clients REST endpoint, brings up a dedicated sipgo+diago stack with those
// credentials, and gets back a *UAS whose inbound channel is private to the
// test. With no shared mutable state in the routing path, t.Parallel() is
// safe — each test can place its own call(s) toward its own SIP URI without
// stepping on anyone else.
//
// Trade-offs:
//   - Each claimUAS call pays ~1s for POST /Clients + REGISTER (measured
//     ~250ms + ~800ms in the spike). Multi-leg tests pay this twice.
//   - We churn /Clients rows. The orphan sweeper at TestMain handles
//     anything that escapes t.Cleanup.
//
// Tests that previously used `currentCall` / `currentCalleeCall` should
// migrate to:
//   uas := claimUAS(t, ctx)
//   call := placeCallTo(ctx, t, uas, verbs)
//
// Multi-leg:
//   caller := claimUAS(t, ctx)
//   callee := claimUAS(t, ctx)
//   call   := placeCallTo(ctx, t, caller, verbs)
//   ... <-callee.Inbound  // when jambonz INVITEs the callee leg

// UAS is a per-test SIP-side endpoint: a freshly-provisioned /Clients user,
// a registered sipgo stack, and a private inbound-call channel. Returned
// from claimUAS. Cleanup is wired via t.Cleanup; callers don't need to
// stop anything explicitly.
type UAS struct {
	// SID is the jambonz Client SID (for diagnostics / explicit deletes).
	SID string
	// Username is the SIP user, suitable for `to.name = <Username>@<domain>`.
	Username string
	// Password is the registration password (rarely needed by tests; the
	// stack is already registered).
	Password string
	// Stack is the underlying sipgo+diago stack. Tests don't normally need
	// this — placeCallTo handles the common case — but it's exposed for
	// scenarios that need to drive a UAC outbound call from this UA.
	Stack *jsip.Stack
	// Inbound delivers exactly one *jsip.Call per inbound INVITE that
	// landed on this UAS. Buffered (cap 4) so jambonz can deliver before
	// the test's select fires.
	Inbound <-chan *jsip.Call
}

// claimUAS provisions a one-shot SIP user, brings up a registered stack,
// and returns a *UAS whose Inbound channel is private to t. Cleanup
// (deregister + delete /Clients row) runs on test exit via t.Cleanup.
//
// Stack lifetime intentionally uses context.Background() — the registration
// loop must outlive the test's bounded ctx, otherwise diago tears the
// REGISTER down the moment the test deadline fires (and the cleanup-time
// "unregister: transaction canceled" warning becomes a real failure path).
// We call Stack.Stop() in the cleanup, which sends an explicit unregister
// before closing the UA.
func claimUAS(t *testing.T, ctx context.Context) *UAS {
	t.Helper()
	sid, username, password := client.ManagedSIPClient(t, ctx)

	inbound := make(chan *jsip.Call, 4)
	stk, err := jsip.Start(context.Background(), jsip.Config{
		SIPDomain: suite.SIPRealm,
		User:      username,
		Pass:      password,
		Transport: "tcp",
		LogLevel:  cfg.LogLevel,
		Resolver:  sipResolver.Resolver(),
	}, func(_ context.Context, call *jsip.Call) error {
		// Best-effort handoff: if the test's select has already picked up
		// or the test ended, drop the call rather than leak the goroutine.
		select {
		case inbound <- call:
		default:
			t.Logf("uas %s: dropping inbound INVITE %s (test inbox full or closed)",
				username, call.CallID())
			_ = call.Reject(486, "Busy Here")
			return nil
		}
		<-call.Done()
		return nil
	})
	if err != nil {
		helperFatalf(t, "claimUAS-stack", "SIP stack start (user=%s): %v", username, err)
	}
	t.Cleanup(func() {
		// Stop the stack BEFORE the test's bounded ctx runs out: this
		// gives diago time to send DEREGISTER before we close the UA.
		// Avoids the cosmetic "Failed to unregister: transaction canceled"
		// log noise the spike noted.
		stk.Stop()
	})
	return &UAS{
		SID:      sid,
		Username: username,
		Password: password,
		Stack:    stk,
		Inbound:  inbound,
	}
}

// claimUAS2 provisions two independent SIP UAs concurrently. Used by
// multi-leg tests (dial, conference, enqueue) that need a caller +
// callee. Each `claimUAS` call costs ~1s for /Clients POST + REGISTER;
// running them in parallel saves ~1s per multi-leg test on the
// critical path.
func claimUAS2(t *testing.T, ctx context.Context) (*UAS, *UAS) {
	t.Helper()
	type result struct {
		ua *UAS
	}
	a := make(chan result, 1)
	b := make(chan result, 1)
	go func() { a <- result{claimUAS(t, ctx)} }()
	go func() { b <- result{claimUAS(t, ctx)} }()
	ra, rb := <-a, <-b
	return ra.ua, rb.ua
}

// placeCallTo (Phase 1) — POSTs /Calls with inline app_json and returns
// the inbound Call that jambonz routes to uas.
//
// Webhook routing: when the webhook tunnel is up, both call_hook and
// call_status_hook point at our public tunnel rather than placeholder
// URLs. This (a) prevents the feature-server log spam from
// `getaddrinfo ENOTFOUND example.invalid` and (b) lets us capture and
// assert on status callbacks (call.status events: trying, ringing,
// in-progress, completed). When the tunnel is down (NGROK_AUTHTOKEN
// unset), we fall back to placeholders — the call_hook still wins
// because app_json takes precedence in feature-server's merge order, but
// status events will fail DNS lookup. That's tolerable when webhook is
// off entirely; the alternative is to require ngrok for Phase-1 too,
// which we don't want.
//
// Status callbacks land on a session named "<testID>-status" registered
// at call placement and released by t.Cleanup. Tests can read them via
// `statusCallbacks(t)` if they want to assert.
func placeCallTo(ctx context.Context, t *testing.T, uas *UAS, verbs []map[string]any, extras ...func(*provision.CallCreate)) *jsip.Call {
	t.Helper()
	blob, err := json.Marshal(verbs)
	if err != nil {
		helperFatalf(t, "marshal-verbs", "%v", err)
	}
	hook, statusHook := callbackURLs(t)
	body := provision.CallCreate{
		CallHook:       hook,
		CallStatusHook: statusHook,
		AppJSON:        string(blob),
		From:           "441514533212",
		To: provision.CallTarget{
			Type: "user",
			Name: fmt.Sprintf("%s@%s", uas.Username, suite.SIPRealm),
		},
		Tag: map[string]any{
			webhook.CorrelationKey: t.Name(),
		},
		TimeLimit:                20,
		SpeechSynthesisVendor:    "deepgram",
		SpeechSynthesisLabel:     deepgramLabel,
		SpeechSynthesisLanguage:  "en-US",
		SpeechSynthesisVoice:     deepgramVoice,
		SpeechRecognizerVendor:   "deepgram",
		SpeechRecognizerLabel:    deepgramLabel,
		SpeechRecognizerLanguage: "en-US",
	}
	for _, e := range extras {
		e(&body)
	}
	return submitAndAwaitOn(ctx, t, body, uas)
}

// callbackURLs returns the call_hook and call_status_hook URLs to set on a
// POST /Calls. When the webhook tunnel is up, both point at it (so jambonz
// can actually deliver status events) and a session is registered for the
// test name so the events are captured. When the tunnel is down, returns
// placeholder URLs that fail DNS lookup — fine for tests that don't care
// about status events but produces feature-server log noise.
//
// Idempotent: if a session for t.Name() already exists (e.g. a Phase-2
// test created one explicitly), reuses it; cleanup is registered exactly
// once via Registry.Release internal idempotency.
func callbackURLs(t *testing.T) (call, status *provision.Webhook) {
	t.Helper()
	if !webhookOn {
		ph := &provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"}
		return ph, ph
	}
	// Make sure a session exists for this test so status events have
	// somewhere to land. If the test already registered one (Phase-2 path)
	// this is a no-op via the Registry's "lookup or create" semantics in
	// New — but Registry.New currently always creates, so guard with
	// Lookup first.
	//
	// Deliberately NOT registering t.Cleanup(Release): jambonz fires the
	// final status callback (`completed`) ~1s AFTER our BYE, by which
	// point most tests have already returned. Releasing the session on
	// test exit means those late callbacks land in `_anon` and we can't
	// correlate them. TestMain teardown reaps the registry on suite end;
	// the per-test sessions are tiny and don't leak meaningfully.
	if _, ok := webhookReg.Lookup(t.Name()); !ok {
		webhookReg.New(t.Name())
	}
	pub := webhookSrv.PublicURL()
	return &provision.Webhook{URL: pub + "/hook", Method: "POST"},
		&provision.Webhook{URL: pub + "/status", Method: "POST"}
}

// statusCallbacks pulls every status callback observed for the current test
// off its session within `within`. Useful for asserting on jambonz's call-
// lifecycle events (trying, ringing, in-progress, completed). Returns nil
// when webhook is off — caller is expected to skip the assertion in that
// mode.
func statusCallbacks(t *testing.T, within time.Duration) []webhook.Callback {
	t.Helper()
	if !webhookOn {
		return nil
	}
	sess, ok := webhookReg.Lookup(t.Name())
	if !ok {
		return nil
	}
	all := DrainCallbacks(sess, within)
	out := make([]webhook.Callback, 0, len(all))
	for _, cb := range all {
		if cb.Hook == "call_status_hook" {
			out = append(out, cb)
		}
	}
	return out
}

// placeWebhookCallTo (Phase 2) — POSTs /Calls with application_sid=webhookApp
// targeting uas, and correlates the resulting webhook traffic back to
// session via the `tag` field. See placeWebhookCall (verbsmain_test.go) for
// the correlation rationale.
func placeWebhookCallTo(ctx context.Context, t *testing.T, uas *UAS, session *webhook.Session, extras ...func(*provision.CallCreate)) *jsip.Call {
	t.Helper()
	requireWebhook(t)
	body := provision.CallCreate{
		ApplicationSID: webhookApp,
		From:           "441514533212",
		To: provision.CallTarget{
			Type: "user",
			Name: fmt.Sprintf("%s@%s", uas.Username, suite.SIPRealm),
		},
		Tag: map[string]any{
			webhook.CorrelationKey: session.ID(),
		},
		TimeLimit: 30,
	}
	for _, e := range extras {
		e(&body)
	}
	return submitAndAwaitOn(ctx, t, body, uas)
}

// placeWebhookCallToNoWait is placeWebhookCallTo without the inbound-INVITE
// wait — for the side of a multi-leg test that doesn't need the *jsip.Call
// (it'll be picked up off uas.Inbound by a goroutine instead). Returns the
// jambonz call_sid so the caller can DeleteCall it later.
func placeWebhookCallToNoWait(ctx context.Context, t *testing.T, uas *UAS, session *webhook.Session, extras ...func(*provision.CallCreate)) string {
	t.Helper()
	requireWebhook(t)
	body := provision.CallCreate{
		ApplicationSID: webhookApp,
		From:           "441514533212",
		To: provision.CallTarget{
			Type: "user",
			Name: fmt.Sprintf("%s@%s", uas.Username, suite.SIPRealm),
		},
		Tag: map[string]any{
			webhook.CorrelationKey: session.ID(),
		},
		TimeLimit: 20,
	}
	for _, e := range extras {
		e(&body)
	}
	sid, err := client.CreateCall(ctx, body)
	if err != nil {
		helperFatalf(t, "create-call", "%v", err)
	}
	return sid
}

// submitAndAwaitOn POSTs the call body, then blocks on uas.Inbound for the
// resulting INVITE. Common tail of placeCallTo + placeWebhookCallTo.
func submitAndAwaitOn(ctx context.Context, t *testing.T, body provision.CallCreate, uas *UAS) *jsip.Call {
	t.Helper()
	sid := client.ManagedCall(t, ctx, body)
	t.Logf("created call sid=%s -> sip:%s@%s", sid, uas.Username, suite.SIPRealm)
	select {
	case call := <-uas.Inbound:
		return call
	case <-ctx.Done():
		helperFatalf(t, "await-inbound", "uas=%s: %v", uas.Username, ctx.Err())
		return nil
	}
}

// WarmupPause is the duration we tell jambonz to pause right after it
// answers the call, before any media-producing or DTMF-detecting verb
// runs. Gives the harness time to:
//
//   - exchange ACK and see the dialog confirmed
//   - latch RTP (symmetric-RTP pinhole opens on first outbound packet)
//   - spin up the PCMU decoder for StartRecording
//
// Without it the first ~200-400ms of TTS/playback lands before we can
// capture it and gets clipped, or jambonz's DTMF detector arms after we've
// already started sending 2833 and misses every digit.
//
// The warmup always takes the explicit form [answer, pause] so the
// receiver's first cross-network event is a deterministic 200 OK rather
// than whichever verb happened to be first. (Many verbs answer implicitly
// — that's fine in prod, but for tests we want the handshake pinned.)
const WarmupPause = 1

// WithWarmup prepends `[answer, pause 1s]` to verbs so jambonz:
//
//  1. sends the 200 OK immediately (explicit `answer` verb),
//  2. idles one second while our recording / DTMF-receive pipeline spins up,
//  3. then runs the real verbs.
//
// Use for any script whose audio or mid-call DTMF content the test
// asserts on.
func WithWarmup(verbs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(verbs)+2)
	out = append(out, V("answer"))
	out = append(out, V("pause", "length", WarmupPause))
	out = append(out, verbs...)
	return out
}

// WithWarmupScript is WithWarmup for webhook.Script (Phase-2 call hooks).
func WithWarmupScript(s webhook.Script) webhook.Script {
	out := make(webhook.Script, 0, len(s)+2)
	out = append(out, V("answer"))
	out = append(out, V("pause", "length", WarmupPause))
	out = append(out, s...)
	return out
}

// --- session + URL helpers --------------------------------------------------
//
// These collapse the 6-line "register webhook session" + "build hook URL with
// X-Test-Id query param" boilerplate that every Phase-2 test was repeating.
// Forgetting the X-Test-Id query param on hooks that don't carry customerData
// (eventHook, toolHook) silently routes callbacks to the shared `_anon`
// session and breaks parallel runs.

// helperFatalf is the canonical "setup helper failure" entry point: it
// records the failure into the FAILURE SUMMARY block, then t.Fatalf's so
// the test stops at the call site. Use this instead of raw `t.Fatalf` in
// any function called from tests that doesn't have a *StepCtx in scope
// (e.g. claimUAS, resolveFixture, submitAndAwaitOn). Without this the
// failure bypasses the summary and is invisible under `-parallel`.
func helperFatalf(t *testing.T, step, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	recordFailure(t, step, msg)
	t.Fatalf("[helper:%s] %s", step, msg)
}

// claimSession registers a webhook session keyed on t.Name() and returns
// (testID, sess). t.Cleanup releases it. Idempotent: re-claiming an
// existing session returns the existing one. Most Phase-2 tests open with
// `testID, sess := claimSession(t)` and never need to touch webhookReg
// directly.
func claimSession(t *testing.T) (string, *webhook.Session) {
	t.Helper()
	id := t.Name()
	sess := webhookReg.New(id)
	t.Cleanup(func() { webhookReg.Release(id) })
	return id, sess
}

// SessionURL returns a per-test webhook callback URL that carries the
// X-Test-Id query param so the server's correlation layer routes the
// callback to this test's session even when the payload itself doesn't
// include customerData (agent eventHook/toolHook, transcribe's
// transcriptionHook, tag verb, etc).
//
// `verb` is the path suffix under /action/ — e.g. "agent-turn",
// "agent-tool", "agent-complete", "gather", "dial".
//
// Use this consistently in place of building the URL by hand.
func SessionURL(sess *webhook.Session, verb string) string {
	return webhookSrv.PublicURL() + "/action/" + verb +
		"?" + webhook.CorrelationHeader + "=" + url.QueryEscape(sess.ID())
}

// SessionAckEmpty registers an empty action-hook script for the named
// verb on `sess`, so the server returns `[]` rather than the default
// hangup-everywhere behaviour. Used by every Phase-2 test that wires an
// actionHook the test wants to capture but doesn't want to chain
// follow-up verbs on. Variadic so common cases (agent: complete + turn)
// fit on one line.
func SessionAckEmpty(sess *webhook.Session, verbs ...string) {
	for _, v := range verbs {
		sess.ScriptActionHook(v, webhook.Script{})
	}
}

// --- timing constants -------------------------------------------------------
//
// Hard-coded magic durations were sprinkled across 12+ tests with the same
// rationale comment repeated. Centralised here so all callers move
// together and the rationale lives once.

// RecognizerArmDelay is the silence we pad after Answer + SendSilence
// before user audio starts, to let the cluster's STT recognizer fully
// arm. Empirically: 500ms loses the first word of a 4-word phrase.
// 700ms is the smallest pad that reliably captures the full phrase
// while shaving ~800ms per arm site (used 11+ times across the suite).
const RecognizerArmDelay = 700 * time.Millisecond

// LLMReplyWindow is how long we wait after sending the user prompt for
// the LLM round-trip + TTS streaming to complete and the recording to
// capture the full reply. 12s gives ~2s of end-of-utterance + ~3s LLM
// + ~5s TTS + 2s slack — earlier shaving to 8/10s caused turn-1
// echoes to drop "hello"/"how" because the recording cut off mid-stream.
const LLMReplyWindow = 12 * time.Second

// BridgeSettleDelay is the delay between caller answer and the callee
// starting to speak in multi-leg tests, so the bridge's RTP path
// stabilises (symmetric-RTP latch on both legs) before audio starts.
const BridgeSettleDelay = 1500 * time.Millisecond

// EndedDrainTimeout is the budget for WaitState(StateEnded) at the end
// of a test — the recording flushes only when the dialog fully tears
// down. Generous because BYE round-trip can take 1-2s on a NAT'd path.
const EndedDrainTimeout = 5 * time.Second

// WaitFor logs a Step (start + ok), sleeps for d, then closes. Use for
// "wait-for-stt", "wait-for-llm-reply", "wait-into-greeting" patterns
// where the only thing the step does is pace.
func WaitFor(t *testing.T, name string, d time.Duration) {
	t.Helper()
	s := Step(t, name)
	defer s.Done()
	time.Sleep(d)
}

// provisionWebhookApp creates an Application bound to the suite-wide
// webhook tunnel and the suite's Deepgram speech credential, registers
// a Cleanup, and returns the application_sid. Used by UAC tests
// (`answer`, `sip:decline`) to set up an `sip:app-<sid>@<realm>` target
// they can dial. Without this helper, every UAC test rebuilds the same
// 14-field ApplicationCreate body.
func provisionWebhookApp(t *testing.T, ctx context.Context, suffix string) string {
	t.Helper()
	return client.ManagedApplication(t, ctx, provision.ApplicationCreate{
		Name:       provision.Name(suffix),
		AccountSID: suite.AccountSID,
		CallHook: provision.Webhook{
			URL:    webhookSrv.PublicURL() + "/hook",
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    webhookSrv.PublicURL() + "/status",
			Method: "POST",
		},
		SpeechSynthesisVendor:    "deepgram",
		SpeechSynthesisLabel:     deepgramLabel,
		SpeechSynthesisVoice:     deepgramVoice,
		SpeechRecognizerVendor:   "deepgram",
		SpeechRecognizerLabel:    deepgramLabel,
		SpeechRecognizerLanguage: "en-US",
	})
}

// HangupAndWaitEnded hangs the call up (best-effort) and blocks for up
// to EndedDrainTimeout for the recorder to flush. Replaces the 5-line
// pattern at the tail of every recording-bearing test.
func HangupAndWaitEnded(t *testing.T, ctx context.Context, call *jsip.Call) {
	t.Helper()
	s := Step(t, "hangup-and-wait-ended")
	defer s.Done()
	_ = call.Hangup()
	endCtx, cancel := context.WithTimeout(ctx, EndedDrainTimeout)
	defer cancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
}

// --- audio round-trip -------------------------------------------------------

// AudioRoundtrip captures the "answer + record + silence + arm-stt + send-wav
// + wait-reply" sequence that 9 tests reproduce verbatim.
//
// Caller still owns the call lifecycle (placeWebhookCallTo + final Hangup);
// AudioRoundtrip drives the in-the-middle steps and returns the recording
// path the caller can hand to AssertTranscript* helpers.
//
// Steps (each gets its own log entry):
//
//	answer-record-and-silence
//	wait-for-stt        (RecognizerArmDelay)
//	send-prompt-wav     (the user's "voice")
//	wait-for-reply      (LLMReplyWindow by default)
//
// The caller wraps the test with `defer HangupAndWaitEnded(...)` and
// finishes with AssertTranscriptHasMost / AssertTranscriptContains.
type AudioRoundtripOpts struct {
	// PromptWAV: caller-provided path to a telephony-grade WAV that gets
	// streamed at the cluster as the user's voice. Required.
	PromptWAV string
	// RecordTag: filename prefix under t.TempDir() for the inbound
	// recording. Defaults to "reply".
	RecordTag string
	// ReplyWait: how long to silence-pace after the prompt. Defaults to
	// LLMReplyWindow. Set shorter for tests that don't need a full LLM
	// round-trip (e.g. transcribe, gather_speech).
	ReplyWait time.Duration
}

// RunAudioRoundtrip performs the answer/record/prompt/wait sequence. Returns
// the recording path. Hangup is the caller's responsibility (use
// HangupAndWaitEnded).
func RunAudioRoundtrip(t *testing.T, ctx context.Context, call *jsip.Call, opts AudioRoundtripOpts) string {
	t.Helper()
	if opts.PromptWAV == "" {
		helperFatalf(t, "audio-roundtrip", "PromptWAV required")
	}
	if opts.RecordTag == "" {
		opts.RecordTag = "reply"
	}
	if opts.ReplyWait == 0 {
		opts.ReplyWait = LLMReplyWindow
	}

	s := Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	rec := filepath.Join(t.TempDir(), opts.RecordTag+".pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(opts.PromptWAV); err != nil {
		s.Fatalf("SendWAV(%s): %v", opts.PromptWAV, err)
	}
	s.Done()

	s = Step(t, "wait-for-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(opts.ReplyWait)
	s.Done()

	return rec
}

// V builds a single jambonz verb as a map[string]any from a verb name plus
// key/value pairs. Replaces the noisy
//
//	map[string]any{"verb": "say", "text": "hello", "loop": 2}
//
// with
//
//	V("say", "text", "hello", "loop", 2)
//
// Panics on odd arg counts or non-string keys — unit test authors will see
// the panic immediately.
func V(verb string, kv ...any) map[string]any {
	if len(kv)%2 != 0 {
		panic("verbs.V: key/value pairs must be even")
	}
	m := map[string]any{"verb": verb}
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			panic("verbs.V: key must be string")
		}
		m[k] = kv[i+1]
	}
	return m
}

// waitEndedOptions controls the post-Answer lifecycle dance.
type waitEndedOptions struct {
	record    bool   // call StartRecording
	silence   bool   // call SendSilence after answer
	recordTag string // filename prefix under t.TempDir(); default "call"
}

// WaitEndedOpt configures AnswerRecordAndWaitEnded.
type WaitEndedOpt func(*waitEndedOptions)

// WithRecord enables StartRecording under t.TempDir() using tag for the file.
func WithRecord(tag string) WaitEndedOpt {
	return func(o *waitEndedOptions) { o.record = true; o.recordTag = tag }
}

// WithSilence starts a SendSilence loop after Answer (NAT latch).
func WithSilence() WaitEndedOpt { return func(o *waitEndedOptions) { o.silence = true } }

// AnswerRecordAndWaitEnded is the common tail of most verb tests: answer,
// optionally start recording, optionally send silence, then block on
// StateEnded. Fatal-errors (via the step context) on any sub-step that
// fails so the failure line names the outer step. For flows that need to
// interleave actions between Answer and StateEnded (e.g. gather), call the
// individual Call methods directly.
//
// Returns the recording path when WithRecord was used (empty string
// otherwise) so callers can hand it to AssertTranscriptContains.
func AnswerRecordAndWaitEnded(s *StepCtx, ctx context.Context, call *jsip.Call, opts ...WaitEndedOpt) string {
	s.t.Helper()
	o := waitEndedOptions{recordTag: "call"}
	for _, opt := range opts {
		opt(&o)
	}
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	var wav string
	if o.record {
		wav = filepath.Join(s.t.TempDir(), o.recordTag+".pcm")
		if err := call.StartRecording(wav); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
	}
	if o.silence {
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
	}
	if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
		s.Fatalf("wait end: %v", err)
	}
	return wav
}

// AssertAudioDuration checks call.AudioDuration() falls within [minDur,maxDur]
// (maxDur=0 means no upper bound) and that RMS is non-trivial when audio is
// expected. Logs a one-line summary on success. Failures are reported via
// the step context so the failure line names the step.
func AssertAudioDuration(s *StepCtx, call *jsip.Call, minDur, maxDur time.Duration, tag string) {
	s.t.Helper()
	dur := call.AudioDuration()
	if dur < minDur {
		s.Errorf("%s: too little audio: got %s want >= %s", tag, dur, minDur)
	}
	if maxDur > 0 && dur > maxDur {
		s.Errorf("%s: too much audio: got %s want <= %s", tag, dur, maxDur)
	}
	if dur >= minDur && call.RMS() < 50 {
		s.Errorf("%s: audio suspiciously quiet: rms=%.1f", tag, call.RMS())
	}
	s.Logf("%s: duration=%s pcm_bytes=%d rms=%.1f codec=%s",
		tag, dur, call.PCMBytesIn(), call.RMS(), call.Codec())
}

// AssertTranscriptContains uploads the call's PCMU recording to Deepgram,
// and asserts the transcript contains every substring in wants (match is
// case-insensitive, punctuation-insensitive via stt.Normalize). Skips when
// DEEPGRAM_API_KEY isn't set — a cheap smoke test without creds is better
// than no smoke test. `recording` must be the same path passed to
// WithRecord() (relative to t.TempDir() or absolute). Failures are
// reported via the step context so the failure line names the step.
func AssertTranscriptContains(s *StepCtx, ctx context.Context, recording string, wants ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.Transcribe(ctx, recording)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	for _, want := range wants {
		n := stt.Normalize(want)
		if !strings.Contains(transcript, n) {
			s.Errorf("transcript missing %q (normalized %q)", want, n)
		}
	}
}

// LongestSilenceMS scans a linear-16 little-endian 8 kHz mono PCM file
// and returns the longest contiguous window where the per-sample
// absolute amplitude stayed below `thresh`. Used for SSML break-tag
// tests: a `<break time="500ms"/>` should produce a measurable silence
// gap in the recording.
//
// Implementation note: walks samples in 10ms frames (80 samples each)
// and treats a frame as "silent" if its peak absolute amplitude is
// below thresh. Frame-based smoothing avoids false splits from a
// single noisy sample mid-silence.
func LongestSilenceMS(pcmPath string, thresh int16) (int, error) {
	data, err := os.ReadFile(pcmPath)
	if err != nil {
		return 0, fmt.Errorf("read pcm: %w", err)
	}
	const frameSamples = 80 // 10ms @ 8kHz
	const frameBytes = frameSamples * 2
	if len(data) < frameBytes {
		return 0, nil
	}
	maxRun, cur := 0, 0
	for off := 0; off+frameBytes <= len(data); off += frameBytes {
		var peak int16
		for i := 0; i < frameBytes; i += 2 {
			s := int16(data[off+i]) | int16(data[off+i+1])<<8
			if s < 0 {
				s = -s
			}
			if s > peak {
				peak = s
			}
		}
		if peak < thresh {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	return maxRun * 10, nil // each frame = 10ms
}

// AssertTranscriptContainsInOrder runs Deepgram STT and asserts each
// `wants` substring appears in the transcript AND in the given order
// (each next match must start AFTER the previous match ends). Used by
// `say` array tests where jambonz plays the entries sequentially.
func AssertTranscriptContainsInOrder(s *StepCtx, ctx context.Context, recording string, wants ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.Transcribe(ctx, recording)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	cursor := 0
	for _, want := range wants {
		n := stt.Normalize(want)
		i := strings.Index(transcript[cursor:], n)
		if i < 0 {
			s.Errorf("transcript %q missing %q (or out of order; cursor at offset %d)",
				transcript, want, cursor)
			return
		}
		cursor += i + len(n)
	}
}

// AssertMulawTranscriptHasMost is AssertTranscriptHasMost but uses
// stt.TranscribeMulawWAV (encoding=mulaw).
func AssertMulawTranscriptHasMost(s *StepCtx, ctx context.Context, recording string, minHits int, wants ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.TranscribeMulawWAV(ctx, recording)
	if err != nil {
		s.Fatalf("stt.TranscribeMulawWAV(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	hits := 0
	var missing []string
	for _, want := range wants {
		if strings.Contains(transcript, stt.Normalize(want)) {
			hits++
		} else {
			missing = append(missing, want)
		}
	}
	if hits < minHits {
		s.Errorf("transcript %q matched only %d/%d (need %d). missing=%v",
			transcript, hits, len(wants), minHits, missing)
	}
}

// AssertMulawTranscriptContains is AssertTranscriptContains but uses
// stt.TranscribeMulawWAV (encoding=mulaw) for files captured from the
// listen/stream verb's WS audio path.
func AssertMulawTranscriptContains(s *StepCtx, ctx context.Context, recording string, wants ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.TranscribeMulawWAV(ctx, recording)
	if err != nil {
		s.Fatalf("stt.TranscribeMulawWAV(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	for _, want := range wants {
		n := stt.Normalize(want)
		if !strings.Contains(transcript, n) {
			s.Errorf("transcript missing %q (normalized %q)", want, n)
		}
	}
}

// AssertTranscriptHasAnyOf runs Deepgram STT on the recording and
// asserts the transcript contains EXACTLY ONE of the candidate
// substrings. Used by `say` array-random and similar one-of-N tests:
// zero matches → jambonz didn't say any of the alternatives; multiple
// matches → it said more than one (regression). Both fail.
func AssertTranscriptHasAnyOf(s *StepCtx, ctx context.Context, recording string, candidates ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.Transcribe(ctx, recording)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	hits := 0
	var matched []string
	for _, c := range candidates {
		if strings.Contains(transcript, stt.Normalize(c)) {
			hits++
			matched = append(matched, c)
		}
	}
	switch hits {
	case 0:
		s.Errorf("transcript %q matched none of %v", transcript, candidates)
	case 1:
		// expected case
	default:
		s.Errorf("transcript %q matched multiple candidates %v (want exactly one)",
			transcript, matched)
	}
}

// AssertTranscriptKeywordCount runs Deepgram STT on the recording and
// asserts that `keyword` appears at least `min` times in the transcript.
// Used by play loop/array tests to prove a playback actually ran N
// times rather than once: a regression that ignores `loop` would still
// pass a "contains the keyword" assertion but fail this one.
func AssertTranscriptKeywordCount(s *StepCtx, ctx context.Context, recording, keyword string, min int) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.Transcribe(ctx, recording)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	n := strings.Count(transcript, stt.Normalize(keyword))
	if n < min {
		s.Errorf("transcript %q contained %q %d time(s); want >= %d",
			transcript, keyword, n, min)
	}
}

// AssertTranscriptHasMost is AssertTranscriptContains relaxed for LLM-driven
// reply audio: requires at least minHits of the wants to appear in the
// transcript. Used by agent_test where occasional word drops/substitutions
// from the LLM (or imperfect TTS→STT round-trip) shouldn't fail an
// otherwise-correct echo.
func AssertTranscriptHasMost(s *StepCtx, ctx context.Context, recording string, minHits int, wants ...string) {
	s.t.Helper()
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		return
	}
	transcript, err := stt.Transcribe(ctx, recording)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recording, err)
	}
	s.Logf("transcript: %q", transcript)
	hits := 0
	var missing []string
	for _, want := range wants {
		if strings.Contains(transcript, stt.Normalize(want)) {
			hits++
		} else {
			missing = append(missing, want)
		}
	}
	if hits < minHits {
		s.Errorf("transcript %q matched only %d/%d keywords (need %d). missing=%v",
			transcript, hits, len(wants), minHits, missing)
	} else if len(missing) > 0 {
		s.Logf("transcript matched %d/%d keywords (tolerated misses: %v)",
			hits, len(wants), missing)
	}
}

// AssertAudioBytes is like AssertAudioDuration but gates on raw PCM bytes
// received, useful when upstream duration reporting is unreliable (play
// verb with transcoding). Failures are reported via the step context so
// the failure line names the step.
func AssertAudioBytes(s *StepCtx, call *jsip.Call, minBytes int64, tag string) {
	s.t.Helper()
	if call.PCMBytesIn() < minBytes {
		s.Fatalf("%s: too little audio: %d bytes (want >= %d)", tag, call.PCMBytesIn(), minBytes)
	}
	if call.RMS() < 50 {
		s.Errorf("%s: audio too quiet: rms=%.1f", tag, call.RMS())
	}
	s.Logf("%s: duration=%s pcm_bytes=%d rms=%.1f codec=%s",
		tag, call.AudioDuration(), call.PCMBytesIn(), call.RMS(), call.Codec())
}

// --- SIP message list helpers ---------------------------------------------

// MethodsOf returns the Method field of every entry that has one.
func MethodsOf(ms []jsip.Message) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.Method != "" {
			out = append(out, m.Method)
		}
	}
	return out
}

// StatusesOf returns the StatusCode field of every entry that has one.
func StatusesOf(ms []jsip.Message) []int {
	out := make([]int, 0, len(ms))
	for _, m := range ms {
		if m.StatusCode != 0 {
			out = append(out, m.StatusCode)
		}
	}
	return out
}

// RequireRecvMethods errors if any of the given methods are missing from
// call.Received(). Failures are reported via the step context so the
// failure line names the step.
func RequireRecvMethods(s *StepCtx, call *jsip.Call, methods ...string) {
	s.t.Helper()
	got := MethodsOf(call.Received())
	for _, want := range methods {
		if !slices.Contains(got, want) {
			s.Errorf("expected received method %q; got %v", want, got)
		}
	}
}

// RequireSentStatus errors if the given status code was not sent at least
// once. Failures are reported via the step context so the failure line
// names the step.
func RequireSentStatus(s *StepCtx, call *jsip.Call, status int) {
	s.t.Helper()
	got := StatusesOf(call.Sent())
	if !slices.Contains(got, status) {
		s.Errorf("expected sent status %d; got %v", status, got)
	}
}

// --- webhook callback helpers ---------------------------------------------

// DrainCallbacks pulls all remaining callbacks off the session's pending
// queue within the given budget. Best-effort; returns nil if the queue is
// already empty.
func DrainCallbacks(sess *webhook.Session, within time.Duration) []webhook.Callback {
	var out []webhook.Callback
	deadline := time.Now().Add(within)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		cb, err := sess.WaitCallback(ctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, cb)
	}
}

// ContainsHook returns true if any callback in cbs is named hook.
func ContainsHook(cbs []webhook.Callback, hook string) bool {
	for _, cb := range cbs {
		if cb.Hook == hook {
			return true
		}
	}
	return false
}

// --- step logging ---------------------------------------------------------

// stepTracker remembers the last Step(name) called per *testing.T so the
// watchdog (see WithTimeout) can report which step was running when the
// budget was exceeded. Writes happen on the test goroutine; reads happen
// from the watchdog goroutine — so the map needs a lock.
//
// timedOut records whether the watchdog has already fired for this test
// (so Done() can suppress a misleading "ok" if the step happens to finish
// after the timeout has been logged).
var (
	stepTrackerMu sync.Mutex
	stepTracker   = map[*testing.T]string{}
	timedOut      = map[*testing.T]bool{}
	// failures captures every Errorf/Fatalf/Fatal/timeout from this run so
	// TestMain can print a one-line-per-failure summary after m.Run() —
	// otherwise, under -parallel and without -v, a failing step's message
	// is buried in interleaved log noise from concurrent tests.
	failures []failureRecord
)

type failureRecord struct {
	testName string
	step     string
	message  string
}

func recordFailure(t *testing.T, step, msg string) {
	stepTrackerMu.Lock()
	failures = append(failures, failureRecord{
		testName: t.Name(),
		step:     step,
		message:  msg,
	})
	stepTrackerMu.Unlock()
}

// PrintFailureSummary writes a one-line-per-failure block. Called from
// TestMain after m.Run() so operators see exactly which test, which step,
// and why — without grepping through interleaved -parallel output.
// Returns the count so TestMain can branch on it.
//
// Writes to ttyOut (the terminal, bypassing go test's stdout/stderr
// capture) so the summary is visible even without -v.
func PrintFailureSummary() int {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	if len(failures) == 0 {
		return 0
	}
	fmt.Fprintf(ttyOut, "\n=== FAILURE SUMMARY (%d) ===\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(ttyOut, "  FAIL %s [step:%s] %s\n", f.testName, f.step, f.message)
	}
	fmt.Fprintln(ttyOut, "============================")
	return len(failures)
}

// ttyOut is the operator's view that bypasses go test's output capture.
//
// Without -v, go test reassigns os.Stdout/os.Stderr to a buffer and only
// prints captured output when a test fails. That hides our heartbeat and
// failure summary unless something fails — defeating their purpose.
//
// Workaround: open /dev/tty directly. The test framework can rebind the
// `os.Stderr` *variable* but it can't unhook fd 2 from the controlling
// terminal. Writing to /dev/tty goes straight to the operator's terminal,
// uncaptured.
//
// In CI / non-tty environments /dev/tty doesn't exist; fall back to
// os.Stderr (which is fine — CI captures everything anyway).
var ttyOut = func() io.Writer {
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		return f
	}
	return os.Stderr
}()

// --- heartbeat ------------------------------------------------------------
//
// Without -v, Go's test runner buffers per-test output until the test
// finishes. With our 25-second multi-leg tests, that means the operator
// sees nothing for 25 seconds, then a wall of text — indistinguishable
// from a hang. The heartbeat goroutine prints a one-line status every
// few seconds: how many tests are running RIGHT NOW, how many done,
// how many failed, and which tests are currently in flight (with the
// step they're on). Quiet enough not to spam, loud enough to prove the
// suite is alive.
//
// We tap into WithTimeout(t, ...) because every test calls it at the top.
// A t.Cleanup hook marks the test "done" when the test exits.

var (
	heartbeatMu        sync.Mutex
	heartbeatActive    = map[string]time.Time{} // testName → start time
	heartbeatCompleted int
	heartbeatPassed    int
	heartbeatFailed    int
)

func heartbeatTestStarted(t *testing.T) {
	heartbeatMu.Lock()
	heartbeatActive[t.Name()] = time.Now()
	heartbeatMu.Unlock()
}

func heartbeatTestFinished(t *testing.T) {
	heartbeatMu.Lock()
	delete(heartbeatActive, t.Name())
	heartbeatCompleted++
	if t.Failed() {
		heartbeatFailed++
	} else {
		heartbeatPassed++
	}
	heartbeatMu.Unlock()
}

// StartHeartbeat begins printing a status line to stderr every interval
// until the returned stop function is called. Safe to call from TestMain
// after m.Run() begins (which is impossible — m.Run blocks). Call BEFORE
// m.Run() and stop AFTER. Returns a stop func to defer.
//
// Output format (one line, no newline-stripping):
//
//	[heartbeat 23s] running=4 done=12 (12 pass, 0 fail) | now: TestVerb_Conference_TwoParty[step:wait-listener-settles]@8s, TestVerb_Say_LongText[step:assert-transcript]@2s, ...
//
// The "now:" suffix lists every active test with its current step name
// and how long it's been running. When the suite is healthy, the line
// changes every interval as tests finish. When something is stuck, you
// see the same test+step lingering — instant signal.
func StartHeartbeat(interval time.Duration) (stop func()) {
	start := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				printHeartbeat(time.Since(start))
			}
		}
	}()
	return func() { close(done) }
}

func printHeartbeat(elapsed time.Duration) {
	heartbeatMu.Lock()
	running := len(heartbeatActive)
	completed := heartbeatCompleted
	passed := heartbeatPassed
	failed := heartbeatFailed
	// snapshot active tests for printing without holding the lock during
	// step lookup (which takes a different lock)
	type active struct {
		name string
		age  time.Duration
	}
	actives := make([]active, 0, running)
	now := time.Now()
	for name, started := range heartbeatActive {
		actives = append(actives, active{name: name, age: now.Sub(started)})
	}
	heartbeatMu.Unlock()

	// Sort actives by age descending — oldest-running first, since those
	// are the most likely to be stuck.
	for i := 0; i < len(actives); i++ {
		for j := i + 1; j < len(actives); j++ {
			if actives[j].age > actives[i].age {
				actives[i], actives[j] = actives[j], actives[i]
			}
		}
	}

	var nowSuffix string
	if len(actives) > 0 {
		var parts []string
		for _, a := range actives {
			step := lookupCurrentStepByName(a.name)
			if step == "" {
				step = "(no step)"
			}
			parts = append(parts, fmt.Sprintf("%s[step:%s]@%s",
				a.name, step, a.age.Round(time.Second)))
		}
		nowSuffix = " | now: " + strings.Join(parts, ", ")
	}

	fmt.Fprintf(ttyOut, "[heartbeat %s] running=%d done=%d (%d pass, %d fail)%s\n",
		elapsed.Round(time.Second), running, completed, passed, failed, nowSuffix)
}

// lookupCurrentStepByName scans the stepTracker map for a test by name.
// Slightly indirect — the map is keyed by *testing.T, not name — but the
// heartbeat only runs every few seconds, so the linear scan is fine.
func lookupCurrentStepByName(name string) string {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	for t, step := range stepTracker {
		if t.Name() == name {
			return step
		}
	}
	return ""
}

func recordCurrentStep(t *testing.T, name string) {
	stepTrackerMu.Lock()
	stepTracker[t] = name
	stepTrackerMu.Unlock()
}

func clearCurrentStep(t *testing.T) {
	stepTrackerMu.Lock()
	delete(stepTracker, t)
	stepTrackerMu.Unlock()
}

func lookupCurrentStep(t *testing.T) string {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	if n, ok := stepTracker[t]; ok {
		return n
	}
	return "(none — test hadn't entered any step)"
}

func markTimedOut(t *testing.T) {
	stepTrackerMu.Lock()
	timedOut[t] = true
	stepTrackerMu.Unlock()
}

func isTimedOut(t *testing.T) bool {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	return timedOut[t]
}

func forgetTest(t *testing.T) {
	stepTrackerMu.Lock()
	delete(stepTracker, t)
	delete(timedOut, t)
	stepTrackerMu.Unlock()
}

// StepCtx is a scoped failure-reporting wrapper returned by Step. Call its
// Fatalf / Errorf methods instead of t.Fatalf / t.Errorf so the resulting
// failure line carries the step name. Call Done() at the end of the step
// to emit "[step:<name>] ok (Xms)".
//
//	s := verbs.Step(t, "send-dtmf-1234")
//	if err := call.SendDTMF("1234"); err != nil {
//	    s.Fatalf("SendDTMF: %v", err)
//	}
//	s.Done()
type StepCtx struct {
	t     *testing.T
	name  string
	start time.Time
	ended bool
}

// Step starts a named step: logs "[step:<name>] start" and returns a
// StepCtx whose Done() closes the step with "[step:<name>] ok (Xms)".
//
// Any failure reported via s.Fatalf / s.Errorf is logged as
// "[step:<name>] FAILED: <message>" before the test fails — so an operator
// reading the failure line immediately sees which step broke and why, with
// no need to scan for the last "start" without an "ok".
//
// The step name must match the corresponding bullet in the test's top-of-
// file "Steps:" comment so the operator doesn't need to open the test file
// to understand the flow.
//
// Naming rules (keep this tight so the logs stay greppable):
//   - kebab-case, lowercase ASCII
//   - verb-first, short (e.g. "answer-and-silence", not "we-answer-the-call")
//   - include discriminating values inline when useful ("send-dtmf-1234")
func Step(t *testing.T, name string) *StepCtx {
	t.Helper()
	recordCurrentStep(t, name)
	t.Logf("[step:%s] start", name)
	return &StepCtx{t: t, name: name, start: time.Now()}
}

// Done closes the step and logs "[step:<name>] ok (Xms)". Safe to call
// after a failure (it will silently no-op if the step is already ended by
// a Fatalf). Call inline at the end of the step, NOT via defer — defer
// would reorder every "ok" to the end of the test and wrong-duration each
// one.
//
// If the test's watchdog already fired (see WithTimeout) we suppress the
// "ok" — the test has already been marked FAILED and a trailing "ok"
// would be misleading.
func (s *StepCtx) Done() {
	if s.ended {
		return
	}
	s.ended = true
	clearCurrentStep(s.t)
	if isTimedOut(s.t) {
		return
	}
	s.t.Helper()
	s.t.Logf("[step:%s] ok (%s)", s.name, time.Since(s.start).Round(time.Millisecond))
}

// Fatalf logs "[step:<name>] FAILED: <msg>" and calls t.Fatalf. Use instead
// of t.Fatalf so the failure line names the step.
func (s *StepCtx) Fatalf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Fatalf("[step:%s] FAILED: %s", s.name, msg)
}

// Errorf logs "[step:<name>] FAILED: <msg>" and calls t.Errorf. Use instead
// of t.Errorf so the failure line names the step. Test continues; call
// Done() at the end of the step as usual — but since this step has already
// been marked failed, Done() will no-op (no misleading "ok" after a FAILED).
func (s *StepCtx) Errorf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Errorf("[step:%s] FAILED: %s", s.name, msg)
}

// Fatal is Fatalf with a plain message.
func (s *StepCtx) Fatal(args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprint(args...)
	recordFailure(s.t, s.name, msg)
	s.t.Fatalf("[step:%s] FAILED: %s", s.name, msg)
}

// Logf passes through to t.Logf without step decoration. Use for
// information-only mid-step logs (payload dumps, recording paths, etc.).
func (s *StepCtx) Logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf(format, args...)
}

// GoroutineFailf is the equivalent of StepCtx.Errorf for code that runs in
// a goroutine spawned by a test (e.g. multi-leg callee handlers). The
// goroutine doesn't have a StepCtx in scope, but failures from it must
// still land in the end-of-run summary. label is the goroutine-step name
// (e.g. "callee:trying").
//
// Usage:
//
//	go func() {
//	    if err := c.Trying(); err != nil {
//	        GoroutineFailf(t, "callee:trying", "Trying: %v", err)
//	        return
//	    }
//	    ...
//	}()
//
// Equivalent to a StepCtx.Errorf in terms of output and summary
// participation.
func GoroutineFailf(t *testing.T, label, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	recordFailure(t, label, msg)
	t.Errorf("[%s] FAILED: %s", label, msg)
}

// WithTimeout is the single source of truth for per-test budget. It
// returns a context whose deadline is `budget` from now and arms a
// watchdog that hard-fails the test if the budget + a small safety margin
// is exceeded. Cleanup runs on test exit (pass or fail), cancelling the
// context and stopping the watchdog.
//
//	func TestVerb_Foo(t *testing.T) {
//	    ctx := WithTimeout(t, 30*time.Second)
//	    ...
//	}
//
// (Name is WithTimeout, not TestTimeout, because go-test treats any
// top-level func whose name starts with "Test" as a test function and
// would reject a signature that doesn't match func(*testing.T).)
//
// When the watchdog fires (i.e. a test blocked past its budget):
//   - the failing line reads
//     "[test-timeout] FAILED: exceeded 30s (last step: send-dtmf-1234)"
//   - the test is marked FAIL via t.Errorf
//   - the test goroutine is forcibly unwound via runtime.Goexit so the
//     suite can continue rather than hanging until the go-test-level
//     "panic: test timed out after Xm" kills the entire binary.
//
// The watchdog's safety margin (2s) covers the case where a context-aware
// call returns just after the deadline — we only consider the test
// actually-stuck when it hasn't cleaned up by budget+2s.
func WithTimeout(t *testing.T, budget time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)

	// Heartbeat: register this test as in-flight. The cleanup below
	// removes it and updates the pass/fail tally.
	heartbeatTestStarted(t)

	// Safety margin covers the common case where a context-aware call
	// returns just after the deadline; we only consider the test
	// actually-stuck when it hasn't finished by budget+safety.
	const safetyMargin = 2 * time.Second
	watchdog := time.AfterFunc(budget+safetyMargin, func() {
		markTimedOut(t)
		lastStep := lookupCurrentStep(t)
		msg := fmt.Sprintf("exceeded %s (last step: %s)", budget, lastStep)
		recordFailure(t, lastStep, "[test-timeout] "+msg)
		t.Errorf("[test-timeout] FAILED: %s", msg)
		// NOTE: runtime.Goexit in this callback ONLY unwinds the timer's
		// own goroutine — not the test goroutine that may be stuck in a
		// non-context-aware syscall. What we can do reliably is mark the
		// test FAILED with a specific reason. If the test goroutine
		// really is wedged in a syscall that ignores our ctx, go-test's
		// own -timeout will eventually kill the binary; the watchdog at
		// least guarantees the failure reason is visible in the log at
		// budget+safetyMargin rather than at go-test's 10-minute alarm.
	})

	t.Cleanup(func() {
		watchdog.Stop()
		cancel()
		heartbeatTestFinished(t)
		forgetTest(t)
	})
	return ctx
}
