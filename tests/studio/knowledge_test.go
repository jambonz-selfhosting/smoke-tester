//go:build studio

package studiotests

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

// TestStudio_ToolAndKnowledge: a pipeline agent answers from its knowledge base and from an HTTP tool in one call.
// The knowledge-base fact is a code made up for this run, so the agent can only say it by searching.
//
// Steps:
//   - knowledge-base: an OpenAI embedding credential, a knowledge base and one indexed document
//   - search: Studio's own search finds the code, so a miss in the call is the call's
//   - call: the caller asks for the code, then for an order's status, then says goodbye
func TestStudio_ToolAndKnowledge(t *testing.T) {
	if cfg.OpenAIAPIKey == "" {
		t.Skip("OPENAI_API_KEY not set: the knowledge base needs an embedding key")
	}
	t.Parallel()
	ctx := WithTimeout(t, 5*time.Minute)
	v := voice(t, ctx)
	code := fmt.Sprintf("%d %d %d %d", 2+rand.IntN(8), 2+rand.IntN(8), 2+rand.IntN(8), 2+rand.IntN(8))

	s := Step(t, "knowledge-base")
	credID, err := ws.CreateEmbeddingCredential(ctx, itName(t, "embed"), "openai", cfg.OpenAIAPIKey)
	if err != nil {
		s.Fatalf("embedding credential: %v", err)
	}
	cleanupLog(t, "embedding credential", func(c context.Context) error { return ws.DeleteEmbeddingCredential(c, credID) })
	kbID, err := ws.CreateKnowledgeBase(ctx, itName(t, "kb"), credID, "text-embedding-3-small")
	if err != nil {
		s.Fatalf("knowledge base: %v", err)
	}
	cleanupLog(t, "knowledge base", func(c context.Context) error { return ws.DeleteKnowledgeBase(c, kbID) })
	doc := fmt.Sprintf(`Harbor Street branch

The Harbor Street branch of Acme Storage is at 12 Harbor Street.
The gate code for the Harbor Street branch is %s. Members type it on the keypad at the vehicle gate.
The branch office is open 8 am to 6 pm on weekdays and closed on Sundays.
`, code)
	docID, err := ws.UploadDocument(ctx, kbID, "harbor-street.txt", doc)
	if err != nil {
		s.Fatalf("upload: %v", err)
	}
	ictx, icancel := context.WithTimeout(ctx, 90*time.Second)
	defer icancel()
	if err := ws.WaitIndexed(ictx, kbID, docID); err != nil {
		s.Fatalf("%v", err)
	}
	s.Done()

	s = Step(t, "search")
	var hits any
	if err := ws.Do(ctx, http.MethodPost, "/knowledge-bases/"+kbID+"/search", map[string]any{"query": "Harbor Street gate code", "k": 3}, &hits); err != nil {
		s.Fatalf("search: %v", err)
	}
	if !strings.Contains(fmt.Sprint(hits), code) {
		s.Fatalf("searching for the gate code did not return it: %v", hits)
	}
	s.Done()

	prompt := `You are the front desk of Acme Storage, on the phone. Keep every reply to one or two short sentences.
Answer questions about a branch only from the knowledge base: search it, never guess.
For an order's status, call get_order_status with the order number.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`
	name := itName(t, "agent")
	id, issues, err := ws.CreatePipelineAgent(ctx, name, studio.Pipeline{
		PromptID: newPrompt(t, ctx, prompt), ToolIDs: []string{desk.ToolID}, Hangup: true, Voice: v,
		Edit: func(c map[string]any) {
			studio.Sub(c, "knowledge")["bases"] = []map[string]any{{"kb_id": kbID, "pin": false}}
		},
	})
	if id != "" {
		cleanupLog(t, "agent", func(c context.Context) error { return ws.DeleteAgent(c, id, name) })
	}
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		t.Fatalf("draft errors: %+v", issues)
	}

	r := call(t, ctx, studio.Scenario{
		Name: "tool-and-knowledge", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, what is the gate code for the Harbor Street branch?"},
			{Say: "Thanks. Can you also check the status of my order, number 5 5 1 2?"},
			{Say: "Great, that's all. Goodbye."},
		},
		MaxSec: 150, TailSec: 12,
		Expect: &studio.Expect{
			ToolsOK: []string{studio.KBToolName, "get_order_status"},
			Heard:   []string{`(?i)shipped`},
		},
	})

	// The agent's own text, not a transcript of its audio: ASR mishears spoken digits ("three" as "third").
	s = Step(t, "check-code")
	want := strings.ReplaceAll(code, " ", "")
	said := false
	for _, x := range turnsOf(t, ctx, r.CallSID) {
		if x.Role == "agent" && strings.Contains(digitsOf(x.Text), want) {
			said = true
		}
	}
	if !said {
		s.Errorf("the agent never said the gate code %s from the knowledge base", want)
	}
	s.Done()
}

// digitsOf keeps the digits of s, spelled-out ones included.
func digitsOf(s string) string {
	s = strings.NewReplacer("zero", "0", "one", "1", "two", "2", "three", "3", "four", "4", "five", "5",
		"six", "6", "seven", "7", "eight", "8", "nine", "9").Replace(strings.ToLower(s))
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}
