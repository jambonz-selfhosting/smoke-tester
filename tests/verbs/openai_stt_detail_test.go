//go:build detail

package verbs

import (
	"testing"
)

// Same flow on gpt-4o-transcribe, which endpoints server-side: pins that adding
// the client-endpointed family did not disturb it.
func TestVerb_Gather_Speech_OpenAI_ServerVad(t *testing.T) {
	gatherWithOpenaiRecognizer(t, serverEndpointedRecognizer)
}
