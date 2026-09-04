//go:build leaderboard

// TTS latency leaderboard — reproduces the measurement behind
// https://jambonz.org/blog/text-to-speech-latency-the-jambonz-leaderboard
// against a live jambonz cluster, for every TTS vendor that has a working
// speech credential on it.
//
// Build-tagged `leaderboard` so it never runs as part of the release-gate
// suite: it places ~40 real calls, burns real vendor credits, and takes
// tens of minutes.
//
//	go test -count=1 -tags leaderboard -run TestTTSLeaderboard \
//	  -parallel 5 -timeout 90m -v ./tests/verbs
//
// Vendors run concurrently, their own cells sequentially — see the loop in
// TestTTSLeaderboard for what that does and does not isolate. `-parallel 1`
// gives a fully sequential sweep at roughly an hour.
//
// # What is measured
//
// Two numbers per utterance, from two independent vantage points:
//
//   - ttfb_ms — the vendor-side number, scraped from the feature-server log
//     over ssh. For vendors with a mediajam streaming dialect this is
//     `variable_tts_time_to_first_byte_ms` (true time-to-FIRST-byte, what
//     the blog reports). For vendors that synthesize node-side instead
//     (microsoft, aws — see ttsStreamingSupported in feature-server's
//     lib/tasks/tts-task.js) the only number available is speech-utils'
//     `tts rtt time ...` line, which is time-to-LAST-byte; the report
//     labels those rows so the two are never silently compared. This is
//     the blog's own caveat about Google, just moved to the vendors that
//     still have it.
//
//     A streaming `say` produces NO server-side number: feature-server hands
//     text to the TTS stream and mediajam plays whatever comes back, with no
//     playback-start event to read a TTFB off. `say.stream` rows are
//     end-to-end only, and that is not a gap in this test.
//
//   - first_audio_ms — end-to-end, black-box: how long after jambonz was
//     told to speak the first non-silent RTP packet reached our UA. The
//     script puts a deterministic pause before every say, so each quiet run
//     in the recording is `that pause + that utterance's latency` — one
//     number per utterance, for every vendor and both modes. It includes
//     mediajam's vendor dial plus a network hop, so it is always larger
//     than ttfb_ms. It is what the caller actually hears, and it is what
//     the report ranks on.
//
// # Shape of a run
//
// One call per (vendor, mode, prompt class). Each call speaks all N prompts
// of its class back to back, so sample 1 is a COLD vendor connection and
// samples 2..N are WARM (mediajam reuses the socket; a streaming say reuses
// the whole TTS stream). The report keeps the two apart — that split is
// more informative than the blog's flat mean and is the reason the numbers
// here can beat it.
//
// Every say sets disableTtsCache so no sample is ever served from the
// cluster's TTS cache.
//
// # Modes
//
//   - "say"        — plain `say`, HTTP webhook app.
//   - "say.stream" — `say` with stream:true, which feature-server only
//     accepts on a WebSocket app (see say.js exec), so those calls go
//     through wsApp. Skipped for vendors whose descriptor has
//     streamingEvents: null (microsoft, aws, whisper, nvidia, resemble).
//
// # Requirements
//
//   - NGROK_AUTHTOKEN (webhook + ws app), as for any Phase-2 test.
//   - ssh access to the feature-server host for the vendor-side number.
//     Host defaults to `bastion`; override with TTS_LB_SSH. Set
//     TTS_LB_SSH=none to run with the end-to-end number only.
//
// # Knobs
//
//	TTS_LB_SSH       ssh host holding the feature-server log (default "bastion", "none" disables)
//	TTS_LB_LOG       path to the log on that host (default ~/.pm2/logs/jambonz-feature-server.log)
//	TTS_LB_VENDORS   comma-separated vendor allow-list (default: every credential with use_for_tts)
//	TTS_LB_MODES     comma-separated subset of "say,say.stream"
//	TTS_LB_CLASSES   comma-separated subset of "short,long"
//	TTS_LB_PROMPTS   prompts spoken per call (default 5, max len(shortPrompts))
//	TTS_LB_OUT       output directory for the report (default <repo>/recordings/tts-leaderboard)
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// --- matrix -----------------------------------------------------------------

// ttsLBPreferredVoice pins the voice (and the language key the cluster's
// voice catalogue files it under) per vendor, so a rerun is comparable to
// the last one. Chosen to match the blog's line-up where the vendor still
// offers that voice; otherwise the vendor's flagship en-US voice. Vendors
// absent here — and pins the vendor has since retired — fall back to
// ttsLBDiscoverVoice, which takes the first voice under the vendor's most
// English-looking language.
//
// cartesia and rimelabs are deliberately unpinned: cartesia names voices by
// uuid, and rimelabs no longer lists the blog's "Abby".
var ttsLBPreferredVoice = map[string]struct{ voice, language string }{
	"deepgram":     {"aura-2-asteria-en", "en-us"},
	"deepgramflux": {"flux-alexis-en", "en-us"},
	"elevenlabs":   {"hpp4J3VqNfWAUOO0d1Us", "en"}, // Bella
	"inworld":      {"Olivia", "en"},
	"murf":         {"en-US-alina", "en-US"},
	"xai":          {"eve", "en"},
	"google":       {"en-US-Wavenet-C", "en-US"},
	"microsoft":    {"en-US-AvaMultilingualNeural", "en-US"},
	"aws":          {"Joanna", "en-US"},
}

// ttsLBNoStreaming are the vendors whose feature-server descriptor has
// streamingEvents: null (or no descriptor at all), so `stream: true` cannot
// work for them — feature-server synthesizes node-side and plays a file.
var ttsLBNoStreaming = map[string]bool{
	"microsoft": true,
	"aws":       true,
	"whisper":   true,
	"nvidia":    true,
	"resemble":  true,
}

// ttsLBShortPrompts — five brief customer-service phrases, 14-20 words, per
// the blog's "short audio" class.
var ttsLBShortPrompts = []string{
	"Hello and thank you for calling. How can I assist you today?",
	"I can help with that. Please tell me your account number and I will pull up your details.",
	"Thanks for holding. Your request has been submitted and you will get a confirmation shortly.",
	"I am sorry for the trouble. Let me connect you with a specialist who can sort this out.",
	"Your appointment is confirmed for Tuesday morning. Is there anything else I can help with?",
}

// ttsLBLongPrompts — five extended IVR-style prompts, 40-60 words, per the
// blog's "long audio" class.
var ttsLBLongPrompts = []string{
	"Thank you for calling customer support. Your call may be recorded for quality and training purposes. " +
		"To check the status of an existing order, press one. To speak with someone about billing, press two. " +
		"For technical support, press three. To hear these options again, please stay on the line.",
	"I have located your account and I can see that your most recent payment was received last Thursday. " +
		"The balance shown on your statement reflects charges from the previous billing period, and any " +
		"credits applied since then will appear on the next statement you receive at the end of the month.",
	"Before we continue I need to verify a few details for security purposes. Please have your account number " +
		"and the last four digits of the card on file available. Once I have confirmed those details I can " +
		"make changes to your plan, update your address, or arrange a callback at a time that suits you.",
	"Our offices are open Monday through Friday from eight in the morning until six in the evening, and on " +
		"Saturday from nine until one. We are closed on public holidays. If you are calling outside those " +
		"hours you can leave a message after the tone and a member of the team will return your call.",
	"I have gone ahead and scheduled the engineer visit for Wednesday between ten and twelve. You will receive " +
		"a text message the evening before with a narrower arrival window, and the engineer will call you " +
		"about thirty minutes before arriving. Please make sure someone over eighteen is at the property.",
}

// --- results ----------------------------------------------------------------

// ttsLBGapSec is the pause scripted between two says. It is the yardstick
// the end-to-end measurement is read against, so it has to be far longer
// than any pause a TTS voice puts inside a sentence — cartesia was observed
// emitting 830ms prosodic pauses, which a 1s gap could not be told apart
// from. At 3s the two never overlap.
const ttsLBGapSec = 3

// ttsLBGapMinMS is the shortest quiet run accepted as an inter-say gap.
// A real one is ttsLBGapSec plus the utterance's latency, so it can never
// fall below 3000ms; the margin below only absorbs frame quantization.
const ttsLBGapMinMS = 2500

// ttsLBSample is one measured utterance.
type ttsLBSample struct {
	Vendor   string `json:"vendor"`
	Voice    string `json:"voice"`
	Mode     string `json:"mode"`  // "say" | "say.stream"
	Class    string `json:"class"` // "short" | "long"
	Index    int    `json:"index"` // 0-based position in the call; 0 == cold
	Chars    int    `json:"chars"`
	CallSID  string `json:"call_sid"`
	TTFBMs   int    `json:"ttfb_ms"`        // -1 when not scraped
	Metric   string `json:"metric"`         // "first-byte" | "last-byte" | ""
	FirstAud int    `json:"first_audio_ms"` // -1 when not applicable
	Note     string `json:"note,omitempty"`
}

var (
	ttsLBMu      sync.Mutex
	ttsLBSamples []ttsLBSample
)

func ttsLBRecord(s ...ttsLBSample) {
	ttsLBMu.Lock()
	defer ttsLBMu.Unlock()
	ttsLBSamples = append(ttsLBSamples, s...)
}

// --- the test ---------------------------------------------------------------

// TestTTSLeaderboard walks (vendor x mode x prompt class), places one call
// per cell, and writes a leaderboard report at the end.
//
// Steps (per cell):
//  1. script-says — [answer, pause, say x N (disableTtsCache), hangup]
//  2. place-call — POST /Calls against the webhook or ws app
//  3. answer-record-and-wait-end — record PCM, send silence, block on end
//  4. measure-first-audio — leading silence of the recording minus warmup
//  5. scrape-vendor-latency — ssh + grep the feature-server log for callSid
func TestTTSLeaderboard(t *testing.T) {
	requireWebhook(t)

	vendors := ttsLBVendors(t)
	if len(vendors) == 0 {
		s := Step(t, "resolve-vendors")
		s.Fatalf("no TTS-capable speech credentials found on the cluster")
	}
	modes := ttsLBSubset("TTS_LB_MODES", []string{"say", "say.stream"})
	classes := ttsLBSubset("TTS_LB_CLASSES", []string{"short", "long"})
	n := ttsLBPromptCount()

	t.Logf("tts leaderboard: %d vendors x %v x %v, %d prompts per call",
		len(vendors), modes, classes, n)
	t.Cleanup(func() { ttsLBWriteReport(t) })

	// Vendors run concurrently (bounded by -parallel), their own cells
	// sequentially. Nearly all of a cell's wall clock is jambonz playing an
	// utterance out after the measurement has already been taken, so a
	// sequential sweep spends the best part of an hour idle. Splitting on
	// vendor keeps the numbers honest where it matters: no vendor account
	// ever sees two of our calls at once, and no cell competes with another
	// cell of the same vendor. Cross-vendor load on the media server is the
	// one thing that is shared — raise -parallel for speed, drop it to 1
	// for a fully isolated run.
	for _, v := range vendors {
		t.Run(v.vendor, func(t *testing.T) {
			t.Parallel()
			for _, mode := range modes {
				if mode == "say.stream" && ttsLBNoStreaming[v.vendor] {
					t.Logf("%s: no mediajam streaming dialect — skipping say.stream", v.vendor)
					continue
				}
				for _, class := range classes {
					t.Run(mode+"/"+class, func(t *testing.T) {
						ttsLBRunCell(t, v, mode, class, n)
					})
				}
			}
		})
	}
}

// ttsLBRunCell places one call and harvests its samples.
func ttsLBRunCell(t *testing.T, v ttsLBVendor, mode, class string, n int) {
	prompts := ttsLBShortPrompts
	budget := 3 * time.Minute
	if class == "long" {
		prompts = ttsLBLongPrompts
		budget = 6 * time.Minute
	}
	if n < len(prompts) {
		prompts = prompts[:n]
	}
	ctx := WithTimeout(t, budget)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-says")
	synth := map[string]any{"vendor": v.vendor, "voice": v.voice, "language": v.language}
	script := make(webhook.Script, 0, len(prompts)*2+1)
	for i, p := range prompts {
		say := map[string]any{
			"verb":            "say",
			"text":            p,
			"synthesizer":     synth,
			"disableTtsCache": true,
		}
		if mode == "say.stream" {
			say["stream"] = true
		}
		script = append(script, say)
		if i < len(prompts)-1 {
			script = append(script, V("pause", "length", ttsLBGapSec))
		}
	}
	script = append(script, V("hangup"))
	sess.ScriptCallHook(WithWarmupScript(script))
	s.Done()

	s = Step(t, "place-call")
	limit := int(budget/time.Second) - 15
	sid, call := ttsLBPlaceCall(ctx, t, uas, sess, mode, limit)
	t.Logf("%s/%s/%s: call_sid=%s voice=%s", v.vendor, mode, class, sid, v.voice)
	s.Done()

	started := time.Now()
	s = Step(t, "answer-record-and-wait-end")
	tag := fmt.Sprintf("ttslb-%s-%s-%s", v.vendor, strings.ReplaceAll(mode, ".", "-"), class)
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	// End-to-end, one number per utterance: the recording opens with the
	// warmup pause and carries a 1s pause before every later say, so each
	// quiet run is `pause + this utterance's latency`.
	var firstAudio []int
	s = Step(t, "measure-first-audio")
	if wav != "" {
		gaps, err := ttsLBSilenceGaps(wav, 200, ttsLBGapMinMS)
		if err != nil {
			s.Errorf("silence gaps: %v", err)
		} else {
			// gaps[0] is the warmup pause; the rest are the scripted ones.
			for i, g := range gaps {
				if i == 0 {
					firstAudio = append(firstAudio, g-WarmupPause*1000)
				} else {
					firstAudio = append(firstAudio, g-ttsLBGapSec*1000)
				}
			}
			s.Logf("%s: quiet runs %v → per-utterance first audio %v (ms)", tag, gaps, firstAudio)
			if len(gaps) != len(prompts) {
				// One quiet run per utterance or the mapping is guesswork.
				// The leading run is anchored to answer and stays valid;
				// drop the rest rather than report misaligned numbers.
				s.Logf("%s: expected %d quiet runs, saw %d — keeping the first "+
					"utterance only", tag, len(prompts), len(gaps))
				if len(firstAudio) > 1 {
					firstAudio = firstAudio[:1]
				}
			}
		}
	}
	s.Done()

	// Vendor-side. The playback-stop line carrying the TTFB is written when
	// the segment finishes, and pm2 flushes lazily, so give the log a beat.
	s = Step(t, "scrape-vendor-latency")
	scraped := ttsLBScrape(ctx, t, sid, v.vendor, started, time.Now())
	s.Logf("scraped %d vendor-side latency lines for call %s", len(scraped), sid)
	s.Done()

	out := make([]ttsLBSample, 0, len(prompts))
	for i, p := range prompts {
		smp := ttsLBSample{
			Vendor: v.vendor, Voice: v.voice, Mode: mode, Class: class,
			Index: i, Chars: len(p), CallSID: sid, TTFBMs: -1, FirstAud: -1,
		}
		if i < len(firstAudio) {
			smp.FirstAud = firstAudio[i]
		}
		m, ok := scraped[ttsLBKey(p)]
		if !ok {
			// node-side "tts rtt time" lines carry a length, not the text
			m, ok = scraped[ttsLBCharKey(len(p))]
		}
		if ok {
			smp.TTFBMs, smp.Metric, smp.Note = m.ms, m.metric, m.note
		} else {
			smp.Note = "no vendor-side latency line found"
		}
		out = append(out, smp)
	}
	ttsLBRecord(out...)

	// A cell that measured nothing means the vendor never spoke — a real
	// failure, not a slow one. AudioDuration is no use as the guard: it
	// counts every inbound RTP sample, and the media server keeps sending
	// silence whether or not the TTS ever produced a byte (elevenlabs'
	// streaming WS handshake 403s on this cluster, and the call still
	// carried a full minute of "audio").
	s = Step(t, "assert-vendor-spoke")
	if wav != "" && len(firstAudio) == 0 {
		s.Errorf("%s/%s/%s: recording holds no speech at all (%s of RTP, rms=%.1f) — "+
			"vendor produced nothing", v.vendor, mode, class, call.AudioDuration(), call.RMS())
	}
	s.Done()
}

// ttsLBPlaceCall dispatches to the webhook app or the ws app depending on
// mode. stream:true is rejected by feature-server on a non-ws app.
func ttsLBPlaceCall(ctx context.Context, t *testing.T, uas *UAS, sess *webhook.Session,
	mode string, limitSec int) (string, *jsip.Call) {
	t.Helper()
	app := webhookApp
	if mode == "say.stream" {
		if wsApp == "" {
			helperFatalf(t, "place-call", "wsApp not provisioned — say.stream needs the WebSocket app")
		}
		app = wsApp
	}
	body := provision.CallCreate{
		ApplicationSID: app,
		From:           "441514533212",
		To: provision.CallTarget{
			Type: "user",
			Name: fmt.Sprintf("%s@%s", uas.Username, suite.SIPRealm),
		},
		Tag:       map[string]any{webhook.CorrelationKey: sess.ID()},
		TimeLimit: limitSec,
	}
	return submitAndAwaitOnWithSID(ctx, t, body, uas)
}

// --- vendor discovery -------------------------------------------------------

type ttsLBVendor struct {
	vendor   string
	voice    string
	language string
}

// ttsLBVendors lists every SP-level speech credential flagged use_for_tts
// and pairs it with a voice. Account-level credentials are ignored on
// purpose: the suite account provisions its own labelled deepgram/murf/xai
// credentials, and mixing labelled and unlabelled ones would make the run
// non-reproducible. Unlabelled SP credentials are what an unlabelled
// synthesizer override resolves to (call-session.js getSpeechCredentials).
func ttsLBVendors(t *testing.T) []ttsLBVendor {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	path := "/ServiceProviders/" + cfg.SPSID + "/SpeechCredentials"
	blob, err := suite.SPClient.Request(ctx, "GET", path, nil, "", 200)
	if err != nil {
		helperFatalf(t, "resolve-vendors", "list speech credentials: %v", err)
	}
	var creds []struct {
		Vendor      string `json:"vendor"`
		Label       any    `json:"label"`
		UseForTTS   int    `json:"use_for_tts"`
		TTSTestedOK any    `json:"tts_tested_ok"`
	}
	if err := json.Unmarshal(blob, &creds); err != nil {
		helperFatalf(t, "resolve-vendors", "decode speech credentials: %v", err)
	}

	allow := map[string]bool{}
	for _, v := range ttsLBSubset("TTS_LB_VENDORS", nil) {
		allow[v] = true
	}

	seen := map[string]bool{}
	var out []ttsLBVendor
	for _, c := range creds {
		if c.UseForTTS != 1 || c.Label != nil || seen[c.Vendor] {
			continue
		}
		if len(allow) > 0 && !allow[c.Vendor] {
			continue
		}
		if len(allow) == 0 && strings.HasPrefix(c.Vendor, "custom:") {
			// Custom vendors need a per-deployment URL + dialect; include
			// one only when it is named explicitly.
			t.Logf("%s: custom vendor — name it in TTS_LB_VENDORS to include it", c.Vendor)
			continue
		}
		if n, ok := c.TTSTestedOK.(float64); ok && n == 0 {
			t.Logf("%s: credential has tts_tested_ok=0 — skipping (fix the credential first)", c.Vendor)
			continue
		}
		seen[c.Vendor] = true
		voice, lang := ttsLBVoiceFor(t, c.Vendor)
		if voice == "" {
			t.Logf("%s: could not resolve a voice — skipping", c.Vendor)
			continue
		}
		out = append(out, ttsLBVendor{vendor: c.Vendor, voice: voice, language: lang})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].vendor < out[j].vendor })
	return out
}

// ttsLBVoiceFor returns the pinned voice for a vendor, or discovers one.
func ttsLBVoiceFor(t *testing.T, vendor string) (voice, language string) {
	t.Helper()
	if p, ok := ttsLBPreferredVoice[vendor]; ok {
		if ttsLBVoiceExists(t, vendor, p.language, p.voice) {
			return p.voice, p.language
		}
		t.Logf("%s: pinned voice %q not offered any more — discovering a replacement", vendor, p.voice)
	}
	return ttsLBDiscoverVoice(t, vendor)
}

type ttsLBLangVoices struct {
	Value  string `json:"value"`
	Voices []struct {
		Value string `json:"value"`
	} `json:"voices"`
}

// ttsLBLanguages fetches the vendor's language/voice tree from the cluster
// (GET .../SpeechCredentials/speech/supportedLanguagesAndVoices), which
// proxies the vendor's own catalogue using the provisioned credential.
func ttsLBLanguages(t *testing.T, vendor string) []ttsLBLangVoices {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	path := "/ServiceProviders/" + cfg.SPSID +
		"/SpeechCredentials/speech/supportedLanguagesAndVoices?vendor=" + vendor
	blob, err := suite.SPClient.Request(ctx, "GET", path, nil, "", 200)
	if err != nil {
		t.Logf("%s: voice catalogue unavailable: %v", vendor, err)
		return nil
	}
	var body struct {
		TTS []ttsLBLangVoices `json:"tts"`
	}
	if err := json.Unmarshal(blob, &body); err != nil {
		t.Logf("%s: decode voice catalogue: %v", vendor, err)
		return nil
	}
	return body.TTS
}

func ttsLBVoiceExists(t *testing.T, vendor, lang, voice string) bool {
	for _, l := range ttsLBLanguages(t, vendor) {
		if !strings.EqualFold(l.Value, lang) {
			continue
		}
		for _, v := range l.Voices {
			if v.Value == voice {
				return true
			}
		}
	}
	return false
}

// ttsLBDiscoverVoice picks the first voice under the most English-looking
// language the vendor offers.
func ttsLBDiscoverVoice(t *testing.T, vendor string) (string, string) {
	langs := ttsLBLanguages(t, vendor)
	for _, want := range []string{"en-us", "en_us", "en", "eng", "auto"} {
		for _, l := range langs {
			if strings.EqualFold(l.Value, want) && len(l.Voices) > 0 {
				return l.Voices[0].Value, l.Value
			}
		}
	}
	for _, l := range langs {
		if strings.HasPrefix(strings.ToLower(l.Value), "en") && len(l.Voices) > 0 {
			return l.Voices[0].Value, l.Value
		}
	}
	return "", ""
}

// --- vendor-side latency scrape --------------------------------------------

type ttsLBMeasure struct {
	ms     int
	metric string // "first-byte" | "last-byte"
	note   string
}

var ttsLBUUID = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// ttsLBSayFile pulls the spoken text out of mediajam's synthesis filename,
// which looks like `say:{session-uuid=..,vendor=..,voice=..}the text`.
var ttsLBSayFile = regexp.MustCompile(`^say:\{[^}]*\}\s*(.*)$`)

// ttsLBRttLine matches speech-utils' node-side synthesis line, the only
// timing available for vendors without a mediajam streaming dialect:
//
//	tts rtt time for 123 chars on microsoft: 412
var ttsLBRttLine = regexp.MustCompile(`tts rtt time for (\d+) chars on ([^:]+): (\d+)`)

// ttsLBScrape reads the feature-server log over ssh and returns the
// vendor-side latency for each utterance of this call, keyed by prompt.
//
// Two shapes are collected:
//
//   - mediajam playback events carrying variable_tts_time_to_first_byte_ms,
//     correlated exactly by callSid and de-duplicated per playback id;
//   - speech-utils "tts rtt time" lines, which the global logger emits
//     WITHOUT a callSid, so they are correlated by vendor + character count
//     within the call's wall-clock window. Sequential cells keep that
//     unambiguous.
func ttsLBScrape(ctx context.Context, t *testing.T, callSID, vendor string,
	from, to time.Time) map[string]ttsLBMeasure {
	t.Helper()
	out := map[string]ttsLBMeasure{}
	host := ttsLBEnv("TTS_LB_SSH", "bastion")
	if host == "none" || host == "" {
		return out
	}
	if !ttsLBUUID.MatchString(callSID) {
		t.Logf("refusing to scrape: call sid %q is not a uuid", callSID)
		return out
	}
	// pm2 buffers; the last playback-stop of the call lands just after BYE.
	time.Sleep(3 * time.Second)

	logPath := ttsLBEnv("TTS_LB_LOG", "$HOME/.pm2/logs/jambonz-feature-server.log")
	remote := fmt.Sprintf(
		`grep -a -e '"callSid":"%s"' -e 'tts rtt time' %s | grep -a -e time_to_first_byte_ms -e 'tts rtt time'`,
		callSID, logPath)

	sctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(sctx, "ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=20", host, remote)
	blob, err := cmd.Output()
	if err != nil && len(blob) == 0 {
		t.Logf("scrape failed (host=%s): %v — end-to-end numbers only for this cell", host, err)
		return out
	}

	seenPlayback := map[string]bool{}
	fromMs, toMs := from.Add(-5*time.Second).UnixMilli(), to.Add(5*time.Second).UnixMilli()

	for _, line := range strings.Split(string(blob), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Time    int64          `json:"time"`
			CallSid string         `json:"callSid"`
			Msg     string         `json:"msg"`
			Evt     map[string]any `json:"evt"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}

		// node-side synthesis (microsoft, aws, ...): no callSid on the line.
		if m := ttsLBRttLine.FindStringSubmatch(rec.Msg); m != nil {
			if rec.Time < fromMs || rec.Time > toMs || !strings.Contains(m[2], vendor) {
				continue
			}
			chars, _ := strconv.Atoi(m[1])
			ms, _ := strconv.Atoi(m[3])
			out[ttsLBCharKey(chars)] = ttsLBMeasure{ms: ms, metric: "last-byte",
				note: "node-side synthesis: time to LAST byte"}
			continue
		}

		if rec.CallSid != callSID || rec.Evt == nil {
			continue
		}
		raw, _ := rec.Evt["variable_tts_time_to_first_byte_ms"].(string)
		ms, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		if id, _ := rec.Evt["variable_tts_playback_id"].(string); id != "" {
			if seenPlayback[id] {
				continue
			}
			seenPlayback[id] = true
		}
		file, _ := rec.Evt["file"].(string)
		m := ttsLBSayFile.FindStringSubmatch(file)
		if m == nil {
			continue
		}
		note := ""
		if hit, _ := rec.Evt["variable_tts_cache_hit"].(string); hit == "true" {
			note = "SERVED FROM CACHE — not a synthesis measurement"
		}
		out[ttsLBKey(m[1])] = ttsLBMeasure{ms: ms, metric: "first-byte", note: note}
	}
	return out
}

// ttsLBKey normalises a prompt so the scraped filename text and the prompt
// we sent hash the same way. Falls back to a character-count key so a
// node-side "tts rtt time" line (which carries only a length) still joins.
func ttsLBKey(text string) string {
	t := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	return "t:" + t
}

func ttsLBCharKey(n int) string { return "t:" + strconv.Itoa(n) }

// --- report -----------------------------------------------------------------

type ttsLBRow struct {
	Vendor    string
	Voice     string
	Mode      string
	Class     string
	Metric    string // vendor-side metric: "first-byte" | "last-byte" | ""
	TTFBCold  int
	TTFBWarm  int
	E2ECold   int
	E2EWarm   int
	N         int
	Missing   int
	CacheHits int
}

func ttsLBWriteReport(t *testing.T) {
	ttsLBMu.Lock()
	samples := append([]ttsLBSample(nil), ttsLBSamples...)
	ttsLBMu.Unlock()
	if len(samples) == 0 {
		t.Log("tts leaderboard: no samples collected — nothing to report")
		return
	}

	dir := ttsLBEnv("TTS_LB_OUT", filepath.Join(cfg.RecordDir, "tts-leaderboard"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("tts leaderboard: mkdir %s: %v", dir, err)
		return
	}
	stamp := time.Now().UTC().Format("20060102-150405")

	// Raw samples, so a rerun can be re-analysed without re-dialling.
	rawPath := filepath.Join(dir, "samples-"+stamp+".json")
	if blob, err := json.MarshalIndent(samples, "", "  "); err == nil {
		if err := os.WriteFile(rawPath, blob, 0o644); err != nil {
			t.Logf("tts leaderboard: write %s: %v", rawPath, err)
		}
	}

	rows := ttsLBAggregate(samples)
	md := ttsLBMarkdown(rows, samples, stamp)
	mdPath := filepath.Join(dir, "leaderboard-"+stamp+".md")
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		t.Logf("tts leaderboard: write %s: %v", mdPath, err)
	}

	csvPath := filepath.Join(dir, "leaderboard-"+stamp+".csv")
	if err := os.WriteFile(csvPath, []byte(ttsLBCSV(rows)), 0o644); err != nil {
		t.Logf("tts leaderboard: write %s: %v", csvPath, err)
	}

	t.Logf("\n%s", md)
	t.Logf("tts leaderboard written:\n  %s\n  %s\n  %s", mdPath, csvPath, rawPath)
}

func ttsLBAggregate(samples []ttsLBSample) []ttsLBRow {
	type key struct{ vendor, mode, class string }
	grouped := map[key][]ttsLBSample{}
	var order []key
	for _, s := range samples {
		k := key{s.Vendor, s.Mode, s.Class}
		if _, ok := grouped[k]; !ok {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], s)
	}
	rows := make([]ttsLBRow, 0, len(order))
	for _, k := range order {
		g := grouped[k]
		row := ttsLBRow{Vendor: k.vendor, Mode: k.mode, Class: k.class, Voice: g[0].Voice,
			TTFBCold: -1, TTFBWarm: -1, E2ECold: -1, E2EWarm: -1, N: len(g)}
		var ttfbWarm, e2eWarm []int
		for _, s := range g {
			if strings.HasPrefix(s.Note, "SERVED FROM CACHE") {
				row.CacheHits++
			}
			if s.TTFBMs < 0 {
				row.Missing++
			} else {
				if s.Metric != "" {
					row.Metric = s.Metric
				}
				if s.Index == 0 {
					row.TTFBCold = s.TTFBMs
				} else {
					ttfbWarm = append(ttfbWarm, s.TTFBMs)
				}
			}
			if s.FirstAud >= 0 {
				if s.Index == 0 {
					row.E2ECold = s.FirstAud
				} else {
					e2eWarm = append(e2eWarm, s.FirstAud)
				}
			}
		}
		row.TTFBWarm = ttsLBMedian(ttfbWarm)
		row.E2EWarm = ttsLBMedian(e2eWarm)
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Mode != rows[j].Mode {
			return rows[i].Mode < rows[j].Mode
		}
		if rows[i].Class != rows[j].Class {
			return rows[i].Class < rows[j].Class
		}
		return ttsLBRank(rows[i]) < ttsLBRank(rows[j])
	})
	return rows
}

// ttsLBRank orders a leaderboard section. Ranking is by the end-to-end warm
// median, because that is the one number every row has: a streaming `say`
// never produces a server-side TTFB (feature-server hands text to the TTS
// stream and mediajam plays what comes back — no playback-start event, so
// nothing to read). Rows with no measurement at all sort last.
func ttsLBRank(r ttsLBRow) int {
	if r.E2EWarm >= 0 {
		return r.E2EWarm
	}
	if r.E2ECold >= 0 {
		return r.E2ECold
	}
	return 1 << 30
}

func ttsLBMedian(v []int) int {
	if len(v) == 0 {
		return -1
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func ttsLBNum(n int) string {
	if n < 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

func ttsLBMarkdown(rows []ttsLBRow, samples []ttsLBSample, stamp string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# jambonz TTS latency leaderboard\n\n")
	fmt.Fprintf(&b, "Run %s UTC against `%s` — %d utterances over %d cells.\n\n",
		stamp, cfg.APIBaseURL, len(samples), len(rows))
	b.WriteString("Two independent measurements per utterance:\n\n" +
		"- **e2e** — end-to-end, black box: from the moment jambonz dispatches the " +
		"`say` to the first non-silent RTP packet at the caller. Includes mediajam's " +
		"vendor dial and one network hop, so it is always larger than the vendor's own " +
		"number — but it is the only one available for every row, and it is what a " +
		"caller actually hears. Sections are ranked on it.\n" +
		"- **ttfb** — the vendor-side number scraped from the feature-server log. " +
		"`first-byte` rows are true time-to-first-byte (the blog's metric); " +
		"`last-byte` rows synthesize node-side and expose only time-to-LAST-byte, so " +
		"they are NOT comparable with the `first-byte` rows. A streaming `say` " +
		"produces no server-side number at all — read `e2e` there.\n\n" +
		"`cold` is the first utterance of a call, on a fresh vendor connection. " +
		"`warm` is the median of the rest, where mediajam reuses the socket and, for " +
		"`say.stream`, the whole TTS stream. Every utterance ran with " +
		"`disableTtsCache`, so none was served from the cluster's TTS cache.\n")

	cur, rank := "", 0
	for _, r := range rows {
		sec := r.Mode + " / " + r.Class
		if sec != cur {
			cur, rank = sec, 0
			fmt.Fprintf(&b, "\n## %s\n\n", sec)
			b.WriteString("| # | vendor | voice | e2e cold ms | e2e warm ms | " +
				"ttfb metric | ttfb cold ms | ttfb warm ms | n |\n")
			b.WriteString("|--:|---|---|--:|--:|---|--:|--:|--:|\n")
		}
		rank++
		metric := r.Metric
		if metric == "" {
			metric = "—"
		}
		fmt.Fprintf(&b, "| %d | %s | `%s` | %s | %s | %s | %s | %s | %d |\n",
			rank, r.Vendor, r.Voice,
			ttsLBNum(r.E2ECold), ttsLBNum(r.E2EWarm), metric,
			ttsLBNum(r.TTFBCold), ttsLBNum(r.TTFBWarm), r.N)
	}

	var warn []string
	for _, r := range rows {
		if r.CacheHits > 0 {
			warn = append(warn, fmt.Sprintf("%s/%s/%s: %d utterance(s) served from the TTS cache",
				r.Vendor, r.Mode, r.Class, r.CacheHits))
		}
		if r.Missing == r.N && r.Mode != "say.stream" {
			warn = append(warn, fmt.Sprintf("%s/%s/%s: no vendor-side latency scraped — "+
				"e2e only for this row", r.Vendor, r.Mode, r.Class))
		}
		if r.E2EWarm < 0 && r.E2ECold < 0 {
			warn = append(warn, fmt.Sprintf("%s/%s/%s: no audio measured at all",
				r.Vendor, r.Mode, r.Class))
		}
	}
	if len(warn) > 0 {
		b.WriteString("\n## Caveats\n\n")
		for _, w := range warn {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	return b.String()
}

func ttsLBCSV(rows []ttsLBRow) string {
	var b strings.Builder
	b.WriteString("mode,class,vendor,voice,e2e_cold_ms,e2e_warm_ms," +
		"ttfb_metric,ttfb_cold_ms,ttfb_warm_ms,n,missing,cache_hits\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s,%s,%s,%d,%d,%d\n",
			r.Mode, r.Class, r.Vendor, r.Voice,
			ttsLBNum(r.E2ECold), ttsLBNum(r.E2EWarm),
			r.Metric, ttsLBNum(r.TTFBCold), ttsLBNum(r.TTFBWarm),
			r.N, r.Missing, r.CacheHits)
	}
	return b.String()
}

// --- small helpers ----------------------------------------------------------

func ttsLBEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// ttsLBSubset reads a comma-separated env override, or returns def.
func ttsLBSubset(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ttsLBPromptCount() int {
	n, err := strconv.Atoi(ttsLBEnv("TTS_LB_PROMPTS", "5"))
	if err != nil || n < 1 {
		return 5
	}
	if n > len(ttsLBShortPrompts) {
		n = len(ttsLBShortPrompts)
	}
	return n
}

// ttsLBSilenceGaps returns the length in ms of every quiet run at or above
// minMS: the leading one, then each run that sits between two utterances.
// The media server sends RTP continuously from answer, so a byte offset in
// the recording is a time axis.
//
// minMS separates a scripted inter-say pause (>= ttsLBGapSec by
// construction) from the prosodic pauses inside a sentence.
func ttsLBSilenceGaps(pcmPath string, thresh int16, minMS int) ([]int, error) {
	data, err := os.ReadFile(pcmPath)
	if err != nil {
		return nil, fmt.Errorf("read pcm: %w", err)
	}
	const frameBytes = 80 * 2 // 10ms @ 8kHz, 16-bit
	var gaps []int
	run, sawAudio := 0, false
	for off := 0; off+frameBytes <= len(data); off += frameBytes {
		var peak int16
		for i := 0; i < frameBytes; i += 2 {
			v := int16(data[off+i]) | int16(data[off+i+1])<<8
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak < thresh {
			run++
			continue
		}
		// Rising edge: close the quiet run that just ended. The leading run
		// counts even if short; later ones must clear minMS to be a pause
		// rather than a breath inside the utterance.
		if !sawAudio || run*10 >= minMS {
			gaps = append(gaps, run*10)
		}
		sawAudio = true
		run = 0
	}
	// A trailing quiet run is the tail after the last utterance, not a gap
	// before one — drop it.
	return gaps, nil
}
