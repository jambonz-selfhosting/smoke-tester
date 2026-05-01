// Tests for the `listen` + `stream` verbs.
//
// Schema: schemas/verbs/listen — required `url` (WS endpoint). jambonz
// opens a WebSocket, sends an initial JSON metadata frame, then streams
// binary audio frames for the lifetime of the session.
//
// Schema: schemas/verbs/stream — alias for `listen` per the schema
// description. Same test shape, different verb name.
//
// Built on the generic webhook WS transport (internal/webhook/ws.go) —
// the same machinery later tests will use for the AsyncAPI ("jambonz
// WebSocket API") and for bidirectional `llm`/`agent` verbs.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Listen_Basic / TestVerb_Stream_Basic — stream audio to a WS
// endpoint and capture frames via the generic webhook WS transport.
//
// Steps (shared by both variants via runListenLikeTest):
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-listen-pause-hangup — [listen|stream ws, pause 12s, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-silence — 200 OK + outbound silence
//  5. wait-for-ws-connect — 1500ms so WS stabilises before speech
//  6. send-wav — stream testdata/test_audio.wav
//  7. post-speech-silence — trailing silence so last frames flush
//  8. hangup-and-wait-ws-close — hangup + wait up to 10s for WS close
//  9. collect-ws-frames — drain captured WS messages
// 10. assert-audio-nontrivial — received audio has >=32 distinct byte values
// 11. save-capture — write raw µ-law to t.TempDir() for offline inspection
func TestVerb_Listen_Basic(t *testing.T) {
	t.Parallel()
	runListenLikeTest(t, "listen")
}

// stream is functionally equivalent to listen per the schema. Run the
// same assertions under the different verb name.
//
// Skipped under `go test -short` because Listen_Basic covers the same
// code path; this variant exists only to confirm the `stream` alias is
// wired. Full release gate runs both.
func TestVerb_Stream_Basic(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping in -short mode: alias of Listen_Basic; full release gate runs both")
	}
	runListenLikeTest(t, "stream")
}

// Test     --POST /Calls-->                       Jambonz
// Webhook  --[listen url=wss://.../ws/<id>]-->    Jambonz
// Jambonz  --INVITE-->                            UAS (answer)
// Jambonz  --WS connect /ws/<id>-->               Webhook (generic WS capture)
// UAS      ==silence + test_audio.wav==>          Jambonz ==> WS (binary frames)
// Test     collects frames, asserts non-silence payload received
// UAS      --BYE-->                               Jambonz (WS closes)
func runListenLikeTest(t *testing.T, verbName string) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	testID, sess := claimSession(t)

	s := Step(t, "script-listen-pause-hangup")
	// ngrok forwards both https and wss over the same host. Swap scheme.
	wsURL := wssURL(webhookSrv.PublicURL(), "/ws/"+testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V(verbName,
			"url", wsURL,
			"mixType", "mono",
			"sampleRate", 8000),
		// Keep the call alive while audio streams to the WS.
		V("pause", "length", 12),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-ws-connect")
	time.Sleep(RecognizerArmDelay)
	s.Done()

	s = Step(t, "send-wav")
	wavPath := resolveFixture(t, speechWAV)
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	// (no explicit wall sleep — the WS-close wait below already bounds
	// the next step and the call's BYE drains buffered audio downstream.)
	s.Done()

	s = Step(t, "hangup-and-wait-ws-close")
	_ = call.Hangup()
	// Wait for the WS to finish, with a cap. Jambonz closes the socket
	// when the session ends.
	select {
	case <-sess.WSClosed():
	case <-time.After(10 * time.Second):
		s.Logf("WS still open after 10s — collecting what's been received")
	}
	s.Done()

	s = Step(t, "collect-ws-frames")
	drainCtx, dcancel := context.WithTimeout(ctx, 2*time.Second)
	defer dcancel()
	msgs := sess.CollectWS(drainCtx)
	s.Logf("WS received %d frames (text=%d, binary=%d)",
		len(msgs), countKind(msgs, webhook.WSText), countKind(msgs, webhook.WSBinary))
	if meta := sess.WSMetadata(); meta != nil {
		s.Logf("WS opening metadata: %v", meta)
	}
	s.Done()

	s = Step(t, "assert-audio-nontrivial")
	audio := webhook.BinaryConcat(msgs)
	if len(audio) == 0 {
		s.Fatalf("WS received zero audio bytes")
	}
	s.Logf("WS audio: %d bytes", len(audio))
	distinct := countDistinctBytes(audio)
	if distinct < 32 {
		s.Errorf("WS audio appears near-silent: only %d distinct byte values", distinct)
	}
	// Bytes-volume floor: the WAV is ~1.7s at 8kHz µ-law (~13.6KB).
	// Require at least 6KB so a regression that ships a fragment of
	// the call's first ~50ms still fails. Stronger than the prior
	// "any audio" check while staying robust against STT-side flakes.
	if len(audio) < 6000 {
		s.Errorf("WS audio too short: %d bytes (want >= 6000)", len(audio))
	}
	s.Done()

	s = Step(t, "save-capture")
	out := filepath.Join(t.TempDir(), testID+".ulaw")
	_ = os.WriteFile(out, audio, 0o644)
	s.Logf("raw µ-law capture: %s", out)
	s.Done()
}

// writeMulawWAV wraps raw µ-law payload bytes in a minimal RIFF/WAVE
// header (PCMU, 8 kHz, mono) so Deepgram's pre-recorded API can decode
// it via stt.Transcribe (which currently passes encoding=linear16; the
// SDK auto-detects µ-law when the WAV header says so).
func writeMulawWAV(path string, payload []byte) error {
	const (
		sampleRate    = 8000
		bitsPerSample = 8
		channels      = 1
		audioFormat   = 7 // µ-law
	)
	dataLen := uint32(len(payload))
	chunkLen := dataLen + 36
	hdr := make([]byte, 0, 44)
	put32 := func(b []byte, v uint32) []byte {
		return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	put16 := func(b []byte, v uint16) []byte {
		return append(b, byte(v), byte(v>>8))
	}
	hdr = append(hdr, "RIFF"...)
	hdr = put32(hdr, chunkLen)
	hdr = append(hdr, "WAVE"...)
	hdr = append(hdr, "fmt "...)
	hdr = put32(hdr, 16)             // fmt chunk size
	hdr = put16(hdr, audioFormat)    // µ-law
	hdr = put16(hdr, channels)
	hdr = put32(hdr, sampleRate)
	hdr = put32(hdr, sampleRate*channels*bitsPerSample/8) // byte rate
	hdr = put16(hdr, channels*bitsPerSample/8)            // block align
	hdr = put16(hdr, bitsPerSample)
	hdr = append(hdr, "data"...)
	hdr = put32(hdr, dataLen)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	_, err = f.Write(payload)
	return err
}

// wssURL swaps an https:// base URL for wss:// and appends path.
func wssURL(base, path string) string {
	u := strings.Replace(base, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u + path
}

// countDistinctBytes counts how many unique byte values appear in b.
// Distinguishes real audio (many) from near-constant silence (few).
func countDistinctBytes(b []byte) int {
	var seen [256]bool
	n := 0
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			n++
		}
	}
	return n
}

func countKind(msgs []webhook.WSMessage, kind webhook.WSKind) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}
