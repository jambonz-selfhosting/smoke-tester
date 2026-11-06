package verbs

import (
	"flag"
	"os"
	"testing"
)

// vendorExtra marks a speech vendor test beyond its core set (credential, say,
// stream, agent, gather, transcribe). A whole-suite run skips it; a -run filter
// (developing that vendor) or SMOKE_VENDOR_FULL=1 runs it.
func vendorExtra(t *testing.T) {
	t.Helper()
	if os.Getenv("SMOKE_VENDOR_FULL") == "1" {
		return
	}
	if f := flag.Lookup("test.run"); testing.Short() || f == nil || f.Value.String() == "" {
		t.Skip("extra vendor coverage: runs with -run <vendor> or SMOKE_VENDOR_FULL=1")
	}
}
