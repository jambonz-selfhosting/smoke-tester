package contract

import (
	"errors"
	"testing"
)

// TestSuccessfulAddSchema — representative smoke test of the validator +
// a common schema. Uses a file that ships with the repo.
func TestSuccessfulAddSchema(t *testing.T) {
	root, err := ResolveSchemasRoot()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	v, err := New(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if err := v.ValidateResponse("rest/common/successful_add.json",
		[]byte(`{"sid":"abc"}`)); err != nil {
		t.Errorf("valid payload rejected: %v", err)
	}
	if err := v.ValidateResponse("rest/common/successful_add.json",
		[]byte(`{}`)); err == nil {
		t.Errorf("empty object unexpectedly passed (sid is required)")
	}
	if err := v.ValidateResponse("rest/common/successful_add.json",
		[]byte(`{"sid":42}`)); err == nil {
		t.Errorf("sid:42 unexpectedly passed (should be string)")
	}
	err = v.ValidateResponse("rest/does_not_exist/nope.json", []byte(`{}`))
	if !errors.Is(err, ErrNoSchema) {
		t.Errorf("missing schema: expected ErrNoSchema, got %v", err)
	}
}

func TestLLMGptLiveEventSchema(t *testing.T) {
	root, err := ResolveSchemasRoot()
	if err != nil {
		t.Fatal(err)
	}
	v, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	const rel = "callbacks/llm-gptlive-event.schema.json"
	valid := []string{
		`{"type":"session.started","event_id":"e1","session":{"id":"live_u7_x","model":"gpt-live-1"}}`,
		`{"type":"session.input_transcript.delta","start_ms":800,"end_ms":1000,"delta":" Hi"}`,
		`{"type":"output_audio.playback_stopped","completion_reason":"interrupted"}`,
		`{"type":"output_audio.playback_started"}`,
		`{"type":"session.delegation.created","delegation":{"id":"item_1","type":"delegation","target":"responses","response_id":"r"}}`,
		`{"type":"response.event","delegation_id":"item_1","event":{"type":"response.completed"}}`,
		`{"type":"error","error":{"type":"invalid_request_error","code":null,"message":"bad"}}`,
		`{"type":"session.closed","reason":"close_requested"}`,
		`{"type":"session.some_future_event"}`,
	}
	for _, body := range valid {
		if err := v.ValidateResponse(rel, []byte(body)); err != nil {
			t.Errorf("valid payload rejected: %s: %v", body, err)
		}
	}
	invalid := []string{
		`{"delta":" Hi"}`,
		`{"type":"session.input_transcript.delta","delta":" Hi"}`,
		`{"type":"session.output_transcript.delta","start_ms":"0","end_ms":200,"delta":"a"}`,
		`{"type":"session.started","session":{"id":"live_u7_x"}}`,
		`{"type":"output_audio.playback_stopped","completion_reason":"cancelled"}`,
		`{"type":"session.delegation.created","delegation":{"id":"item_1","target":"backend"}}`,
		`{"type":"response.event","delegation_id":"item_1"}`,
		`{"type":"error","error":{"code":"x"}}`,
	}
	for _, body := range invalid {
		if err := v.ValidateResponse(rel, []byte(body)); err == nil {
			t.Errorf("invalid payload accepted: %s", body)
		}
	}
}
