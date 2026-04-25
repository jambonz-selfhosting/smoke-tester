# Test audio fixtures

## test_audio.wav

Source: copied from `api-server/data/test_audio.wav` (`testDeepgramStt` in
`lib/utils/speech-utils.js` uses it to smoke-test Deepgram credentials).

Format: RIFF WAVE, linear PCM 16-bit mono 8 kHz, ~1.79 s, ~28 KB.

### test_audio.transcript

The reference transcript of `test_audio.wav`, captured once via Deepgram
nova-3 on 2026-04-20 and pinned here as the source of truth. Tests that
stream this WAV into a jambonz gather verb assert that the recognized
speech matches this text (allowing for minor vendor-side variation).

If the transcript ever needs re-generation:

```
go run ./spikes/002-gather-speech-fixture tests/verbs/testdata/test_audio.wav
```

but do NOT re-generate casually — the whole point of pinning is stability.
