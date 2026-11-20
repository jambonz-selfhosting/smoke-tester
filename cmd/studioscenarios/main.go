// Command studioscenarios plays one scripted caller against an existing Agent Studio agent
// (internal/studio does the work). make test-studio builds its own agents and runs every vendor.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

func main() {
	if err := run(); err != nil {
		log.Printf("studioscenarios: %v", err)
		os.Exit(1)
	}
}

func run() error {
	scFile := flag.String("scenario", "", "scenario JSON")
	outDir := flag.String("out", "", "output dir")
	sidFile := flag.String("sid", "", "file holding a Studio session cookie value")
	account := flag.String("account", "", "Studio account (workspace) id; empty: the login's first workspace")
	apiBase := flag.String("api", "https://studio.jambonz.io/v1", "Studio API base")
	sbc := flag.String("sbc", "56.10.102.117:5060", "SBC host:port")
	fromHost := flag.String("from-host", "123.21.48.183", "inbound-did: From host (a whitelisted carrier IP)")
	flag.Parse()

	sc, err := studio.LoadScenario(*scFile)
	if err != nil {
		return err
	}
	sid, err := os.ReadFile(*sidFile)
	if err != nil {
		return err
	}
	c, err := studio.New(*apiBase, string(sid))
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := c.UseWorkspace(ctx, *account); err != nil {
		return err
	}
	r, err := studio.Run(ctx, c, sc, studio.RunOptions{OutDir: *outDir, SBC: *sbc, FromHost: *fromHost})
	if err != nil {
		return err
	}
	fmt.Printf("== %s call_sid=%s ended_by_agent=%v\n%s\n", sc.Name, r.CallSID, r.EndedByAgent, r.Transcript)
	if sc.Expect == nil {
		return nil
	}
	if len(r.Failures) > 0 {
		return fmt.Errorf("%s FAILED:\n  %s", sc.Name, strings.Join(r.Failures, "\n  "))
	}
	fmt.Printf("== %s PASSED\n", sc.Name)
	return nil
}
