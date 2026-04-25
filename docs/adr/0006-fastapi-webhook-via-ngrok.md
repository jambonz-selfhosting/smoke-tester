# 6. Go `net/http` webhook app exposed via ngrok

- **Status:** Accepted
- **Date:** 2026-04-18
- **Updated:** 2026-04-18 (stack changed from Python/FastAPI to Go/`net/http` following [ADR-0011](0011-go-modules-for-dependencies.md))
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** webhook, networking, go

## Context

jambonz fetches call-control logic ("verbs") from a URL configured on the Application, and posts call-status events to another URL. For the test harness to drive call flows, it must host an HTTP server that the remote jambonz cluster can reach — and it must do this from both a laptop (behind NAT) and a Debian/EC2 box (public IP). Each test also needs to inject a *different* verb script without reprovisioning the Application on every test.

## Decision

Run a Go `net/http` server inside the test process and expose it to the remote jambonz cluster via an **ngrok tunnel**.

- The server exposes `GET /hook` (returns verb JSON), `POST /status` (records status events), and `GET /events/{test_id}` (internal — used by assertions).
- Per-test verb scripts are registered in an in-memory registry keyed by an `X-Test-Id` correlation header, set on the SIP INVITE and propagated to jambonz via custom SIP headers.
- Status events are stored in memory keyed by `call_sid` *and* `X-Test-Id` for fast test-side lookup. `sync.Map` or a mutex-guarded map is fine given the modest event volumes expected.
- ngrok is managed via [`ngrok-go`](https://pkg.go.dev/golang.ngrok.com/ngrok) (the official library) and started once in `TestMain`. If `NGROK_DOMAIN` is set, a reserved domain gives a stable URL across runs; otherwise ngrok issues an ephemeral one.
- The webhook server runs in the same process as the Go test binary — no sidecar, no separate language runtime to manage.
- **jambonz webhooks support both HTTP and WebSocket delivery** (RFC 6455 over the same ngrok tunnel). HTTP is the v1 focus; WebSocket delivery is called out as a future ADR once the v1 suite is green.
- On the Debian/EC2 box where a public IP exists, ngrok is still used by default for URL consistency; a future ADR may switch to binding directly on the public IP if ngrok becomes a bottleneck.

## Consequences

- Positive: uniform tunnel behaviour across laptop and EC2 — tests are written once, run anywhere.
- Positive: in-process Go server + in-memory registries make per-test script injection trivial and race-free (one goroutine, one map, one mutex).
- Positive: ngrok's request-inspection UI is useful during test authoring.
- Positive: no framework dependency — stdlib `net/http` is enough for the handful of routes we need.
- Negative: external dependency (ngrok account, authtoken). Free tier has connection limits that may bite on large parallel runs.
- Negative: ngrok adds latency and a possible failure mode unrelated to jambonz. Failure diagnostics must distinguish "ngrok 502" from "jambonz bug".
- Follow-up: if ngrok reliability becomes a problem, evaluate Cloudflare Tunnel or a direct bind-on-public-IP mode for the Debian box.
- Follow-up: WebSocket webhook delivery (the jambonz alternative to HTTP) deserves its own ADR once v1 is green.

## Alternatives considered

### Option A — Bind the webhook app directly on the test box's public IP
Rejected as the default: only works on the Debian/EC2 box. Would fork the suite's behaviour between environments.

### Option B — Cloudflare Tunnel
Rejected for v1: a good option, but requires a Cloudflare-managed hostname and more setup than ngrok's authtoken. Revisit if ngrok proves problematic.

### Option C — A third-party Go HTTP framework (gin, echo, chi)
Rejected: stdlib `net/http` + `http.ServeMux` covers our three routes fully. A framework would add a dependency without solving a problem we have.

### Option D — Static verb files served per-Application (no per-test injection)
Rejected: would require creating a new Application per test or branching verb logic on request details — both awkward. Per-test script registry is cleaner.

### Option E — WebSocket-only webhook delivery (skip HTTP)
Deferred, not rejected. jambonz supports WebSocket webhook delivery natively. Covering it is a v1+ goal; folding it into v1 adds scope without increasing release-gate confidence on the HTTP path most customers use.

## References

- ADR-0007 (three SIP modes) — the `X-Test-Id` correlation header flows from the UA through jambonz into the webhook app.
- ADR-0008 (run-id tagging) — webhook registry entries use `run_id` as an outer prefix for multi-run isolation.
