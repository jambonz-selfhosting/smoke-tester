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
