//go:build studio

package studiotests

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

var (
	cfg  *config.Settings
	ws   *studio.Client
	conn *studio.Connection
	// jz adds and removes credentials on the jambonz account Studio is connected to.
	jz     *provision.Client
	desk   studio.OrderDesk
	outDir string
	sbc    string
)

func TestMain(m *testing.M) {
	cfg = config.MustLoad()
	if !cfg.HasStudio() {
		fmt.Println("tests/studio: STUDIO_API_URL and STUDIO_SID_FILE (or STUDIO_EMAIL and STUDIO_PASSWORD) not set — nothing to run")
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	code := 1
	cleanup, err := setup(ctx)
	cancel()
	if err != nil {
		log.Printf("tests/studio: %v", err)
	} else {
		code = m.Run()
	}
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

func setup(ctx context.Context) (func(), error) {
	sid := ""
	if cfg.StudioSIDFile != "" {
		b, err := os.ReadFile(cfg.StudioSIDFile)
		if err != nil {
			return nil, fmt.Errorf("STUDIO_SID_FILE: %w", err)
		}
		sid = string(b)
	} else {
		var err error
		if sid, err = studio.SignIn(ctx, cfg.StudioAPIURL, cfg.StudioEmail, cfg.StudioPassword); err != nil {
			return nil, err
		}
	}
	var err error
	if ws, err = studio.New(cfg.StudioAPIURL, sid); err != nil {
		return nil, err
	}
	if err := ws.UseWorkspace(ctx, cfg.StudioAccountID); err != nil {
		return nil, err
	}
	if conn, err = ws.Connection(ctx); err != nil {
		return nil, fmt.Errorf("workspace %s connection: %w", ws.AccountID, err)
	}
	// Credentials go on the account Studio runs its agents on, through the cluster this .env points at.
	if conn.APIBaseURL != cfg.APIBaseURL {
		return nil, fmt.Errorf("the Studio workspace is connected to %s but JAMBONZ_API_URL is %s", conn.APIBaseURL, cfg.APIBaseURL)
	}
	root, err := contract.ResolveSchemasRoot()
	if err != nil {
		return nil, fmt.Errorf("contract schemas root: %w", err)
	}
	v, err := contract.New(root)
	if err != nil {
		return nil, fmt.Errorf("contract validator: %w", err)
	}
	jz = provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", v, provision.WithLabel("sp"))
	sbc = net.JoinHostPort(cfg.SBCPublicIP.String(), "5060")
	outDir = os.Getenv("STUDIO_OUT_DIR")
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "smoke-studio-"+provision.RunID())
	}
	log.Printf("tests/studio: workspace %s on %s (account %s); call artifacts in %s",
		ws.AccountID, conn.Hostname, conn.JambonzAccountSID, outDir)

	var undo []func(context.Context) error
	cleanup := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		for i := len(undo) - 1; i >= 0; i-- {
			if err := undo[i](cctx); err != nil {
				log.Printf("tests/studio: %v", err)
			}
		}
	}
	toolID, created, err := ws.EnsureTool(ctx, studio.OrderStatusTool)
	if err != nil {
		return cleanup, fmt.Errorf("order tool: %w", err)
	}
	if created {
		undo = append(undo, func(c context.Context) error { return ws.DeleteTool(c, toolID) })
	}
	promptID, err := ws.CreatePrompt(ctx, "smoke "+provision.RunID()+": order desk", studio.OrderDeskPrompt)
	if err != nil {
		return cleanup, fmt.Errorf("order-desk prompt: %w", err)
	}
	undo = append(undo, func(c context.Context) error { return ws.DeletePrompt(c, promptID) })
	desk = studio.OrderDesk{PromptID: promptID, ToolID: toolID}
	return cleanup, nil
}
