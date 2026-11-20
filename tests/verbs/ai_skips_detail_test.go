//go:build detail

package verbs

import "testing"

// s2s — synonym for llm. Same credential gating.
func TestVerb_S2S_Basic(t *testing.T) {
	t.Skip("s2s is synonym for llm — same credential gating (Tier 5)")
}

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

// ultravox_s2s — Ultravox model shortcut. Needs ULTRAVOX credential.
func TestVerb_Ultravox_S2S_Basic(t *testing.T) {
	t.Skip("ultravox_s2s needs an Ultravox credential (Tier 5)")
}
