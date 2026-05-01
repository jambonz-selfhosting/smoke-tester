// Skip-stubs for AI verbs that require vendor credentials we don't have
// configured in the test account. Each test documents what it would
// cover so coverage-matrix status stays honest.
//
// All of these are Tier 5 per docs/coverage-matrix.md. Turn the skip
// into a real assertion when the relevant credential is provisioned on
// the test account.
package verbs

import "testing"

// llm — covered by TestVerb_LLM_Deepgram in llm_test.go (uses Deepgram
// Voice Agent via inline auth, the same DEEPGRAM_API_KEY the suite already
// requires).

// s2s — synonym for llm. Same credential gating.
func TestVerb_S2S_Basic(t *testing.T) {
	t.Skip("s2s is synonym for llm — same credential gating (Tier 5)")
}

// agent — covered by TestVerb_Agent_Echo in agent_test.go (uses Deepgram
// STT/TTS via the in-jambonz credential, Deepseek LLM via inline auth).

// dialogflow — connects caller to a Google Dialogflow agent. Needs
// Dialogflow project credentials.
func TestVerb_Dialogflow_Basic(t *testing.T) {
	t.Skip("dialogflow needs a Dialogflow project credential (Tier 5)")
}

// openai_s2s — shortcut for llm with vendor=openai. Needs OPENAI api key.
func TestVerb_OpenAI_S2S_Basic(t *testing.T) {
	t.Skip("openai_s2s needs an OpenAI credential (Tier 5)")
}

// deepgram_s2s — shortcut for llm with vendor=deepgram. Needs a Deepgram
// agent credential separate from our existing DEEPGRAM_API_KEY (which is
// used for pre-recorded STT, not agent/voice).
func TestVerb_Deepgram_S2S_Basic(t *testing.T) {
	t.Skip("deepgram_s2s needs a Deepgram AGENT credential (not our DG STT key) (Tier 5)")
}

// elevenlabs_s2s — ElevenLabs model shortcut. Needs ELEVENLABS credential.
func TestVerb_ElevenLabs_S2S_Basic(t *testing.T) {
	t.Skip("elevenlabs_s2s needs an ElevenLabs credential (Tier 5)")
}

// google_s2s — Google model shortcut. Needs a Google GenAI/Gemini
// credential (distinct from Google TTS/STT).
func TestVerb_Google_S2S_Basic(t *testing.T) {
	t.Skip("google_s2s needs a Google GenAI/Gemini credential (Tier 5)")
}

// ultravox_s2s — Ultravox model shortcut. Needs ULTRAVOX credential.
func TestVerb_Ultravox_S2S_Basic(t *testing.T) {
	t.Skip("ultravox_s2s needs an Ultravox credential (Tier 5)")
}
