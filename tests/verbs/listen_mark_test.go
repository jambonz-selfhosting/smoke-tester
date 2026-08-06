// Tests for the `mark` feature of the `listen` verb's bidirectional
// audio channel.
//
// Spec: https://docs.jambonz.org/verbs/verbs/listen#mark
// Authoritative protocol: freeswitch-modules/modules/mod_audio_fork
// (lws_glue.cpp). The feature-server JS only handles non-streaming
// `playAudio`/`killAudio`; the mark synchronization lives entirely in
// mod_audio_fork's *streaming* playout buffer path
// (bidirectionalAudio.streaming == true).
//
// How marks work on the wire (our WS server → jambonz, then back):
//
//  1. We declare bidirectionalAudio {enabled, streaming:true, sampleRate}.
//     jambonz dials out to our /ws/<id> endpoint.
//  2. We send raw linear16 PCM as BINARY frames. mod_audio_fork appends
//     the samples to a circular playout buffer that is dubbed onto the
//     caller's outbound audio.
//  3. We send a TEXT frame {type:"mark", data:{name}}. The next binary
//     frame prepends an AUDIO_MARKER sentinel into the playout buffer at
//     that position (the mark moves inventory -> in-use).
//  4. As the buffer drains to the caller and the sentinel is reached,
//     mod_audio_fork emits {type:"mark", data:{name, event:"playout"}}
//     back to us. (If a mark is pending and no further audio arrives, it
//     is emitted from inventory immediately — see dub_speech_frame.)
//  5. killAudio / clearMarks discard buffered audio; any pending marks
//     come back as {type:"mark", data:{name, event:"cleared"}}.
//
// Phase-2 + bidirectional: requires NGROK_AUTHTOKEN (webhook tunnel) and
// DEEPGRAM_API_KEY (to synthesize the audio we play back to the caller).
package verbs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Per-test playback phrases. Distinct text per test gives EnsureWAV
// distinct cache keys, so two parallel tests never read/write the same
// cache file (a 0-byte partial-read race otherwise). Both are long
// enough (~several seconds at 8kHz) to span many 20ms frames so the
// playout drain — and, for the cleared test, a deep undrained buffer —
// is observable.
const (
	markPlayoutText = "This audio is being streamed back over the websocket connection to test the mark playout event end to end."
	markClearedText = "This is a much longer block of audio whose only purpose is to fill the playout buffer with many seconds of samples so that a kill audio command arrives well before the marked position could ever drain out to the caller leg of the call."
)

// TestVerb_Listen_Mark_Playout — stream audio back to the caller over a
// bidirectional listen WS, tag it with a `mark`, and assert jambonz
// reports the mark as "playout" once the audio reaches the caller. We
// also record the caller leg and assert real (non-silent) audio actually
// arrived — proving the mark fired because the audio played, not by
// accident.
//
// Steps:
//  1. require-creds — skip without ngrok + Deepgram
//  2. synthesize-playback-audio — Deepgram TTS -> raw 8k linear16 PCM
//  3. register-webhook-session — webhook.Registry.New + cleanup
//  4. script-listen-bidi — [listen ws bidirectionalAudio{streaming}, pause, hangup]
//  5. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  6. answer-and-record — 200 OK + outbound silence (NAT latch) + record
//  7. wait-ws-connect — block until jambonz opens /ws/<id>
//  8. stream-audio-and-mark — binary PCM frames, then {mark name}, then flush frames
//  9. await-playout-mark — assert {type:mark,event:playout,name} comes back
//
// 10. assert-caller-heard-audio — caller recording has real audio (RMS/bytes)
// 11. hangup
func TestVerb_Listen_Mark_Playout(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !tts.HasKey() {
		t.Skip("skipping: DEEPGRAM_API_KEY required to synthesize playback audio")
	}
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "synthesize-playback-audio")
	pcm := marksRawPCM(ctx, t, s, markPlayoutText)
	s.Logf("playback PCM: %d bytes (~%.1fs at 8kHz/16-bit)", len(pcm), float64(len(pcm))/16000.0)
	s.Done()

	s = Step(t, "script-listen-bidi")
	wsURL := wssURL(webhookSrv.PublicURL(), "/ws/"+testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("listen",
			"url", wsURL,
			"mixType", "mono",
			"sampleRate", 8000,
			"bidirectionalAudio", map[string]any{
				"enabled":    true,
				"streaming":  true,
				"sampleRate": 8000,
			}),
		V("pause", "length", 12),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	recPath := marksAnswerRecord(t, call)

	s = Step(t, "wait-ws-connect")
	connCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	if err := sess.WSConnected(connCtx); err != nil {
		ccancel()
		s.Fatalf("WS never connected: %v", err)
	}
	ccancel()
	// Let the latch settle and the playout media bug attach before we
	// start writing samples into the buffer.
	time.Sleep(BridgeSettleDelay)
	s.Done()

	const markName = "mark-playout-1"

	s = Step(t, "stream-audio-and-mark")
	if err := marksStreamPCM(sess, pcm, true); err != nil {
		s.Fatalf("stream PCM: %v", err)
	}
	if err := sess.SendWSText(marksMarkMsg(markName)); err != nil {
		s.Fatalf("send mark: %v", err)
	}
	// The mark sits in inventory until the next binary frame moves it into
	// the playout buffer. Send a short trailing burst of audio so the
	// sentinel is inserted and subsequently drains to the caller.
	if err := marksStreamPCM(sess, pcm[:min(len(pcm), 6400)], true); err != nil {
		s.Fatalf("stream trailing PCM: %v", err)
	}
	s.Done()

	s = Step(t, "await-playout-mark")
	markCtx, mcancel := context.WithTimeout(ctx, 20*time.Second)
	mark, err := sess.WaitWSMark(markCtx, markName)
	mcancel()
	if err != nil {
		s.Fatalf("never received mark %q: %v", markName, err)
	}
	if mark.Event != "playout" {
		s.Errorf("mark %q event = %q, want %q", markName, mark.Event, "playout")
	}
	s.Logf("received mark: name=%q event=%q", mark.Name, mark.Event)
	s.Done()

	s = Step(t, "assert-caller-heard-audio")
	call.StopRecording()
	// The streamed audio is dubbed onto the caller's outbound leg, so it
	// lands in our recording of that leg. Real audio, not silence.
	if rms := call.RMS(); rms < 200 {
		s.Errorf("caller recording near-silent (rms=%.0f); streamed audio may not have played", rms)
	}
	if dur := call.AudioDuration(); dur < 500*time.Millisecond {
		s.Errorf("caller recording too short: %v (want >= 500ms)", dur)
	}
	s.Logf("caller recording: rms=%.0f dur=%v file=%s", call.RMS(), call.AudioDuration(), recPath)
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Listen_Mark_Cleared — stream audio + a mark, then immediately
// kill the buffered audio with `killAudio`. jambonz must report the
// pending mark as "cleared" rather than "playout".
//
// Steps:
//  1. require-creds — skip without ngrok + Deepgram
//  2. synthesize-playback-audio — Deepgram TTS -> raw 8k linear16 PCM
//  3. register-webhook-session
//  4. script-listen-bidi
//  5. place-call
//  6. answer-and-record
//  7. wait-ws-connect
//  8. stream-audio-mark-then-kill — binary PCM, {mark name}, more PCM, {killAudio}
//  9. await-cleared-mark — assert {type:mark,event:cleared,name}
//
// 10. hangup
func TestVerb_Listen_Mark_Cleared(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !tts.HasKey() {
		t.Skip("skipping: DEEPGRAM_API_KEY required to synthesize playback audio")
	}
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "synthesize-playback-audio")
	pcm := marksRawPCM(ctx, t, s, markClearedText)
	s.Done()

	s = Step(t, "script-listen-bidi")
	wsURL := wssURL(webhookSrv.PublicURL(), "/ws/"+testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("listen",
			"url", wsURL,
			"mixType", "mono",
			"sampleRate", 8000,
			"bidirectionalAudio", map[string]any{
				"enabled":    true,
				"streaming":  true,
				"sampleRate": 8000,
			}),
		V("pause", "length", 12),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	_ = marksAnswerRecord(t, call)

	s = Step(t, "wait-ws-connect")
	connCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	if err := sess.WSConnected(connCtx); err != nil {
		ccancel()
		s.Fatalf("WS never connected: %v", err)
	}
	ccancel()
	time.Sleep(BridgeSettleDelay)
	s.Done()

	const markName = "mark-cleared-1"

	s = Step(t, "stream-audio-mark-then-kill")
	// Burst a large block of audio (no pacing) so several seconds of
	// samples are buffered ~instantly, then mark, then one more frame to
	// move the mark into the playout buffer (in-use), then killAudio
	// immediately. The kill arrives while the marked position is still
	// deep in the undrained buffer, so the mark must come back "cleared"
	// rather than "playout".
	if err := marksStreamPCM(sess, pcm, false); err != nil {
		s.Fatalf("stream PCM: %v", err)
	}
	if err := sess.SendWSText(marksMarkMsg(markName)); err != nil {
		s.Fatalf("send mark: %v", err)
	}
	if err := marksStreamPCM(sess, pcm[:min(len(pcm), 3200)], false); err != nil {
		s.Fatalf("stream trailing PCM: %v", err)
	}
	if err := sess.SendWSText(marksKillAudioMsg()); err != nil {
		s.Fatalf("send killAudio: %v", err)
	}
	s.Done()

	s = Step(t, "await-cleared-mark")
	markCtx, mcancel := context.WithTimeout(ctx, 20*time.Second)
	mark, err := sess.WaitWSMark(markCtx, markName)
	mcancel()
	if err != nil {
		s.Fatalf("never received mark %q: %v", markName, err)
	}
	if mark.Event != "cleared" {
		s.Errorf("mark %q event = %q, want %q (killAudio should clear, not play out)",
			markName, mark.Event, "cleared")
	}
	s.Logf("received mark: name=%q event=%q", mark.Name, mark.Event)
	s.Done()

	s = Step(t, "hangup")
	call.StopRecording()
	_ = call.Hangup()
	s.Done()
}

// --- helpers ---------------------------------------------------------

// marksRawPCM synthesizes text to a telephony WAV and returns the raw
// 8kHz/mono/16-bit little-endian PCM (the 44-byte RIFF header stripped),
// ready to stream as binary frames into the bidirectional playout buffer.
func marksRawPCM(ctx context.Context, t *testing.T, s *StepCtx, text string) []byte {
	t.Helper()
	wavPath, err := tts.EnsureWAV(ctx, "testdata/listen", text, tts.PromptOptions{})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		s.Fatalf("read WAV: %v", err)
	}
	if len(wav) <= 44 {
		s.Fatalf("WAV too small: %d bytes", len(wav))
	}
	// EnsureWAV writes a fixed 44-byte canonical RIFF/WAVE header (PCM,
	// 8kHz, mono, 16-bit); the remainder is raw linear16 samples.
	return wav[44:]
}

// marksAnswerRecord answers the call, starts the NAT-latch silence loop,
// and begins recording the caller leg. Returns the recording path.
func marksAnswerRecord(t *testing.T, call *jsip.Call) string {
	t.Helper()
	s := Step(t, "answer-and-record")
	defer s.Done()
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	recPath := t.TempDir() + "/mark-caller.wav"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	return recPath
}

// marksStreamPCM writes raw linear16 PCM to the WS as a sequence of
// binary frames sized to ~20ms (320 bytes at 8kHz/16-bit).
//
// When paced, frames are spaced ~10ms apart so the buffer fills a little
// faster than it drains — a realistic real-time stream where the marked
// audio eventually plays out. When not paced (burst), every frame is sent
// back-to-back so a large block of audio is buffered ~instantly; used by
// the cleared test so a killAudio can arrive while the marked position is
// still deep in the (undrained) playout buffer.
func marksStreamPCM(sess *webhook.Session, pcm []byte, paced bool) error {
	const frameBytes = 320 // 20ms @ 8kHz, 16-bit mono
	if len(pcm) == 0 {
		return errors.New("marksStreamPCM: empty PCM")
	}
	for off := 0; off < len(pcm); off += frameBytes {
		end := off + frameBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := sess.SendWSBinary(pcm[off:end]); err != nil {
			return err
		}
		if paced {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

// marksMarkMsg builds a {type:"mark", data:{name}} text frame.
func marksMarkMsg(name string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "mark",
		"data": map[string]any{"name": name},
	})
	return b
}

// marksKillAudioMsg builds a {type:"killAudio"} text frame.
func marksKillAudioMsg() []byte {
	b, _ := json.Marshal(map[string]any{"type": "killAudio"})
	return b
}
