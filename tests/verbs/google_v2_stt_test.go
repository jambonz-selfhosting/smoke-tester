// Tests for the `gather` verb with STT vendor "google" on the v2 API,
// exercising recognizer.googleOptions:
//
//   - serviceVersion: "v2"
//   - parentPath:     projects/{project}/locations/{location}
//   - recognizerId:   a pre-created v2 recognizer resource
//
// google is an OPTIONAL vendor (see config.HasGoogleSTT /
// provisionGoogleCredential in verbsmain_test.go): TestMain only provisions
// the google SpeechCredential when GOOGLE_STT_KEYFILE is set. When it is
// unset, googleLabel stays "" and these tests pass immediately after a log —
// a plain `return`, never t.Skip, never a failure — so the suite stays green
// with or without the key. Same shape as the speechmatics/xai STT tests.
//
// Why the recognizerId test asserts on language_code and not on words:
//
// In Speech v2 the recognizer resource stores its own RecognitionConfig
// (model, language, adaptation). A StreamingRecognizeRequest that also sends
// an inline `config` overrides those stored fields, so "did the recognizerId
// take effect?" cannot be answered from a transcript alone — a correct
// transcript is exactly what an IGNORED recognizerId produces too, since the
// verb's own language/model would then be in force.
//
// So the test needs a recognizer whose stored language DIFFERS from the
// language the verb asks for (GOOGLE_STT_RECOGNIZER_LANGUAGE vs
// GOOGLE_STT_VERB_LANGUAGE, enforced by config.HasGoogleV2Recognizer), and
// asserts on the `language_code` Google reports back:
//
//	language_code == recognizer's language -> the recognizer resource won  (PASS)
//	language_code == verb's language       -> recognizerId was dropped     (FAIL)
//
// That second case is the bug this suite exists to catch: feature-server used
// to read `rOpts.sgoogleOptions?.recognizerId` (a typo), so
// GOOGLE_SPEECH_RECOGNIZER_ID was never set on the channel, the media server
// fell back to the wildcard recognizer "_" with a full inline config, and
// callers saw the stock v2 model no matter what recognizerId they passed.
package verbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Google_V2_Wildcard — the baseline: v2 with a
// parentPath but no recognizerId, i.e. the wildcard recognizer "_" carrying a
// full inline config. Proves the v2 path itself works (regional endpoint
// routing, explicit LINEAR16 decoding at the call's sample rate) before the
// recognizerId test attributes any failure to recognizerId.
func TestVerb_Gather_Speech_Google_V2_Wildcard(t *testing.T) {
	if !cfg.HasGoogleSTT() || googleLabel == "" {
		t.Log("GOOGLE_STT_KEYFILE not set — passing without exercising google STT v2")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	googleOptions := map[string]any{"serviceVersion": "v2"}
	if cfg.GoogleSTTParentPath != "" {
		googleOptions["parentPath"] = cfg.GoogleSTTParentPath
	}

	cb := runGoogleGather(ctx, t, uas, sess, cfg.GoogleSTTVerbLanguage, googleOptions)

	s := Step(t, "assert-transcript-sun-shining")
	transcript := strings.ToLower(extractTranscript(cb))
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q language_code=%q", transcript, googleLanguageCode(cb))
	// Same tolerance as TestVerb_Gather_Speech: one of the two words is
	// enough on a 1.3s telephony clip.
	if !strings.Contains(transcript, "sun") && !strings.Contains(transcript, "shining") {
		s.Errorf("transcript %q matched neither sun nor shining", transcript)
	}
	s.Done()
}

// TestVerb_Gather_Speech_Google_V2_RecognizerId — name a pre-created v2
// recognizer whose stored language differs from the verb's, and assert the
// reported language_code comes from the RECOGNIZER. See the package comment
// for why language_code is the only assertion that can tell an honored
// recognizerId from an ignored one.
func TestVerb_Gather_Speech_Google_V2_RecognizerId(t *testing.T) {
	if !cfg.HasGoogleV2Recognizer() || googleLabel == "" {
		t.Logf("google v2 recognizer not configured (need GOOGLE_STT_KEYFILE + "+
			"GOOGLE_STT_PARENT_PATH + GOOGLE_STT_RECOGNIZER_ID + GOOGLE_STT_RECOGNIZER_LANGUAGE "+
			"differing from GOOGLE_STT_VERB_LANGUAGE=%q) — passing without exercising recognizerId",
			cfg.GoogleSTTVerbLanguage)
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	cb := runGoogleGather(ctx, t, uas, sess, cfg.GoogleSTTVerbLanguage, map[string]any{
		"serviceVersion": "v2",
		"parentPath":     cfg.GoogleSTTParentPath,
		"recognizerId":   cfg.GoogleSTTRecognizerID,
	})

	s := Step(t, "assert-language-code-came-from-recognizer")
	got := googleLanguageCode(cb)
	if got == "" {
		s.Fatalf("no language_code in action/gather payload — cannot tell whether "+
			"recognizerId %q was honored: %s", cfg.GoogleSTTRecognizerID, string(cb.Body))
	}
	s.Logf("recognizer=%q stored language=%q verb asked for %q; google reported %q (transcript %q)",
		cfg.GoogleSTTRecognizerID, cfg.GoogleSTTRecognizerLang, cfg.GoogleSTTVerbLanguage,
		got, extractTranscript(cb))

	switch {
	case strings.EqualFold(got, cfg.GoogleSTTRecognizerLang):
		// the recognizer resource supplied the config
	case strings.EqualFold(got, cfg.GoogleSTTVerbLanguage):
		s.Fatalf("language_code=%q equals the language the VERB asked for, so recognizerId %q "+
			"was dropped and the wildcard recognizer was used with an inline config "+
			"(expected %q from the recognizer resource)",
			got, cfg.GoogleSTTRecognizerID, cfg.GoogleSTTRecognizerLang)
	default:
		s.Errorf("language_code=%q is neither the recognizer's %q nor the verb's %q",
			got, cfg.GoogleSTTRecognizerLang, cfg.GoogleSTTVerbLanguage)
	}
	s.Done()
}

// runGoogleGather drives one `gather input=[speech]` call with the given
// google recognizer options and returns the action/gather callback. The call
// choreography is identical to TestVerb_Gather_Speech; only the recognizer
// differs, so it lives here once rather than twice.
func runGoogleGather(
	ctx context.Context,
	t *testing.T,
	uas *UAS,
	sess *webhook.Session,
	language string,
	googleOptions map[string]any,
) webhook.Callback {
	t.Helper()

	s := Step(t, "script-gather-speech-google")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", SessionURL(sess, "gather"),
			"recognizer", map[string]any{
				"vendor":        "google",
				"label":         googleLabel,
				"language":      language,
				"googleOptions": googleOptions,
			}),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()
	t.Cleanup(func() { _ = call.Hangup() })

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	// google v2 opens a gRPC stream to a regional endpoint — use the LONG pad
	// so the recognizer is armed before the WAV starts, else the first words
	// get clipped.
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(resolveFixture(t, speechWAV)); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()
	return cb
}

// googleLanguageCode pulls the language Google reported for the recognized
// utterance. feature-server's google normalizer puts it at
// speech.language_code and keeps the raw vendor event under
// speech.vendor.evt, so fall back to the raw copy if the normalized field is
// absent.
func googleLanguageCode(cb webhook.Callback) string {
	if s := cb.NestedString("speech.language_code"); s != "" {
		return s
	}
	return cb.NestedString("speech.vendor.evt.language_code")
}
