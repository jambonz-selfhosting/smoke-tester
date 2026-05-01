// Probe verifies the synthetic-realm SIP-routing model end-to-end:
//
//   1. SP create ephemeral account
//   2. SP create account-scope ApiKey
//   3. account-scope POST /Accounts/<sid>/SipRealms/<synthetic-realm>
//   4. account-scope POST /Clients to provision a UAS user
//   5. install StaticResolver mapping <synthetic-realm> → SBCPublicIP
//   6. start sipgo+diago and REGISTER the UAS
//   7. POST /Calls with to.name=<user>@<synthetic-realm> and wait for the
//      INVITE to land at our UAS
//   8. answer + hangup; tear down the account
//
// If steps 6 and 7 succeed, the model is viable and TestMain can adopt it.
// If they fail, the failure tells us exactly which jambonz layer rejects
// the synthetic realm so we can adjust strategy without burning cycles
// refactoring the full suite.
//
// Run:
//
//	go run ./cmd/probe
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

func main() {
	logLevel := flag.String("log", "info", "log level")
	flag.Parse()
	_ = logLevel

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	schemasRoot, err := contract.ResolveSchemasRoot()
	if err != nil {
		log.Fatalf("schemas: %v", err)
	}
	v, err := contract.New(schemasRoot)
	if err != nil {
		log.Fatalf("contract: %v", err)
	}

	sp := provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", v, provision.WithLabel("sp"))

	// 1. Account
	accountName := provision.Name("probe")
	ctx := withInterrupt(context.Background())
	defer ctxCleanup()

	log.Printf("step 1: create account %s", accountName)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	accountSID, err := sp.CreateAccount(cctx, provision.AccountCreate{
		Name:               accountName,
		ServiceProviderSID: cfg.SPSID,
	})
	cancel()
	if err != nil {
		log.Fatalf("create account: %v", err)
	}
	log.Printf("  account_sid=%s", accountSID)
	defer cleanupAccount(sp, accountSID)

	// 2. ApiKey for the account
	log.Printf("step 2: mint account-scope api key")
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	_, accountToken, err := sp.CreateApiKey(cctx, provision.ApiKeyCreate{AccountSID: accountSID})
	cancel()
	if err != nil {
		log.Fatalf("create api key: %v", err)
	}
	log.Printf("  token=%s…", accountToken[:8])

	acctClient := provision.New(cfg.APIBaseURL, accountToken, accountSID, v,
		provision.WithLabel("probe-acct"))

	// 3. SipRealm
	realm := strings.ToLower(accountName) + "." + cfg.SIPRealmZone
	log.Printf("step 3: set sip_realm=%s", realm)
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	if err := acctClient.SetSipRealm(cctx, accountSID, realm); err != nil {
		cancel()
		log.Fatalf("set sip_realm: %v", err)
	}
	cancel()
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	a, err := sp.GetAccount(cctx, accountSID)
	cancel()
	if err != nil {
		log.Fatalf("verify get account: %v", err)
	}
	if !strings.EqualFold(a.SIPRealm, realm) {
		log.Fatalf("sip_realm not set in DB: got %q want %q", a.SIPRealm, realm)
	}
	log.Printf("  sip_realm in DB: %s ✓", a.SIPRealm)

	// 4. Client
	log.Printf("step 4: create SIP client (UAS user)")
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	username := "probe-uas"
	password := "probe-secret-1234567890abcdef"
	clientSID, err := acctClient.CreateSIPClient(cctx, provision.SIPClientCreate{
		AccountSID: accountSID,
		Username:   username,
		Password:   password,
		IsActive:   "1",
	})
	cancel()
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	log.Printf("  client_sid=%s username=%s", clientSID, username)

	// 5. Static resolver
	log.Printf("step 5: install StaticResolver: %s -> %s", realm, cfg.SBCPublicIP)
	resolver, err := jsip.NewStaticResolver(map[string]string{
		realm: cfg.SBCPublicIP.String(),
	})
	if err != nil {
		log.Fatalf("resolver: %v", err)
	}
	defer resolver.Close()

	// 6. SIP stack + REGISTER
	log.Printf("step 6: start sipgo + REGISTER %s@%s", username, realm)
	inbound := make(chan *jsip.Call, 4)
	stack, err := jsip.Start(ctx, jsip.Config{
		SIPDomain: realm,
		User:      username,
		Pass:      password,
		Transport: "tcp",
		Resolver:  resolver.Resolver(),
	}, func(_ context.Context, call *jsip.Call) error {
		log.Printf("  INVITE received: From=%s To=%s", call.From(), call.To())
		select {
		case inbound <- call:
		default:
		}
		<-call.Done()
		return nil
	})
	if err != nil {
		log.Fatalf("sip stack: %v", err)
	}
	defer stack.Stop()
	log.Printf("  REGISTER ok ✓")

	// 7. POST /Calls and wait for INVITE
	log.Printf("step 7: POST /Calls to=%s@%s", username, realm)
	ictx, icancel := context.WithTimeout(ctx, 30*time.Second)
	placeholder := &provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"}
	callSID, err := acctClient.CreateCall(ictx, provision.CallCreate{
		From: "441514533212",
		To: provision.CallTarget{
			Type: "user",
			Name: fmt.Sprintf("%s@%s", username, realm),
		},
		CallHook:                 placeholder,
		CallStatusHook:           placeholder,
		AppJSON:                  `[{"verb":"answer"},{"verb":"pause","length":1},{"verb":"hangup"}]`,
		TimeLimit:                30,
		SpeechSynthesisVendor:    "google",
		SpeechSynthesisLanguage:  "en-US",
		SpeechSynthesisVoice:     "en-US-Standard-A",
		SpeechRecognizerVendor:   "google",
		SpeechRecognizerLanguage: "en-US",
	})
	icancel()
	if err != nil {
		log.Fatalf("create call: %v", err)
	}
	log.Printf("  call_sid=%s", callSID)

	select {
	case call := <-inbound:
		log.Printf("step 8: answer + hangup")
		if err := call.Answer(); err != nil {
			log.Fatalf("answer: %v", err)
		}
		// Wait until call ends (jambonz hangs up after pause+hangup)
		ectx, ecancel := context.WithTimeout(ctx, 10*time.Second)
		defer ecancel()
		_ = call.WaitState(ectx, jsip.StateEnded)
		log.Printf("  call ended ✓")
	case <-time.After(20 * time.Second):
		log.Fatalf("INVITE never arrived at our UAS — cluster did not route to our synthetic realm")
	}

	log.Printf("PROBE PASSED — synthetic-realm routing works.")
}

var (
	doCleanup []func()
	cleanupMu = make(chan struct{}, 1)
)

func cleanupAccount(sp *provision.Client, sid string) {
	log.Printf("teardown: enumerate clients of %s", sid)
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clients, err := sp.ListSIPClientsForAccount(cctx, sid)
	if err != nil {
		log.Printf("  list clients: %v", err)
	}
	log.Printf("  found %d clients to delete", len(clients))
	for _, cl := range clients {
		if cl.AccountSID != sid {
			continue // double-check
		}
		if err := sp.DeleteSIPClient(cctx, cl.ClientSID); err != nil {
			log.Printf("  delete client %s: %v", cl.ClientSID, err)
		}
	}
	if err := sp.DeleteAccount(cctx, sid); err != nil {
		log.Printf("  delete account: %v", err)
	} else {
		log.Printf("  account %s deleted ✓", sid)
	}
}

// ctxCleanup is a no-op placeholder so callers can defer a cleanup hook.
func ctxCleanup() {}

// withInterrupt returns a context cancelled on SIGINT/SIGTERM so probe
// runs survive Ctrl-C cleanup paths.
func withInterrupt(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	return ctx
}

// ensure go imports stay used in case future edits drop refs.
var (
	_ = net.IPv4
	_ time.Time
)
