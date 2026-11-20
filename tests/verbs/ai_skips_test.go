// Skip-stubs for AI verbs that require vendor credentials we don't have
// configured in the test account. Each test documents what it would
// cover so coverage-matrix status stays honest.
//
// All of these are Tier 5 per docs/coverage-matrix.md. Turn the skip
// into a real assertion when the relevant credential is provisioned on
// the test account.
package verbs

// llm — covered by TestVerb_LLM_Deepgram in llm_test.go (uses Deepgram
// Voice Agent via inline auth, the same DEEPGRAM_API_KEY the suite already
// requires).

// agent — covered by TestVerb_Agent_Echo in agent_test.go (uses Deepgram
// STT/TTS via the in-jambonz credential, Deepseek LLM via inline auth).

// elevenlabs_s2s — the llm verb's elevenlabs path is covered for real by
// TestVerb_LLM_ElevenLabs_DTMF in elevenlabs_llm_s2s_test.go, gated on
// ELEVENLABS_API_KEY.

// google_s2s — covered for real by TestVerb_LLM_Google_ToolHook in
// google_llm_s2s_test.go, gated on GEMINI_API_KEY.
