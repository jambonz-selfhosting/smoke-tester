# Components & traffic

Top-level view of the harness ↔ jambonz.me interaction. Each component,
the stack it's built on, and which protocol it uses to talk to the jambonz
cluster.

## Diagram

```mermaid
flowchart LR
    subgraph Harness["Test harness (smoke-tester, Go)"]
        direction TB
        UA1["SIP UAS — 'caller-uas'<br/>sipgo + diago<br/>TCP :ephemeralA · RTP UDP"]
        UA2["SIP UAS — 'callee-uas'<br/>sipgo + diago<br/>TCP :ephemeralB · RTP UDP"]
        UAC["SIP UAC — outbound origination<br/>sipgo + diago<br/>shares Stack A transport<br/>⚠ deferred — unlocks alert / sip:decline"]:::deferred
        HTTP["HTTP server<br/>net/http ServeMux<br/>/hook /status /action/&lt;verb&gt;"]
        WSS["WebSocket server<br/>gorilla/websocket<br/>/ws/&lt;session-id&gt;"]
        REST["REST client<br/>net/http<br/>POST /v1/*"]
    end

    Ngrok(["ngrok tunnel<br/>wraps HTTP + WS on one public host"]):::tunnel

    subgraph Jambonz["jambonz.me cluster"]
        direction TB
        SBC["drachtio SBC<br/>SIP over TCP · RTP"]
        Feat["feature-server<br/>verb executor"]
        API["api-server<br/>REST /v1"]
        FS["freeswitch<br/>media + mixer"]
    end

    UA1 -- "SIP/TCP :5060 · RTP/UDP" --> SBC
    UA2 -- "SIP/TCP :5060 · RTP/UDP" --> SBC
    UAC -- "SIP/TCP :5060 · RTP/UDP" --> SBC

    REST == "HTTPS /v1/Calls, /Applications, ..." ==> API

    HTTP -.-> Ngrok
    WSS -.-> Ngrok
    Ngrok -- "HTTPS call_hook · action_hook · status_hook" --> Feat
    Ngrok -- "WSS listen/stream audio · AsyncAPI (future)" --> Feat

    API --> Feat
    Feat --> SBC
    SBC <--> FS

    classDef tunnel fill:#ffe9a8,stroke:#b08900,color:#111;
    classDef deferred fill:#eee,stroke:#888,color:#555,stroke-dasharray: 4 3;
```

## Components and protocols

### Harness-side components

| Component | Stack | Traffic | Used by |
|---|---|---|---|
| **SIP UAS — `caller-uas`** (Stack A) | `emiago/sipgo` + `emiago/diago`, ephemeral TCP port per test process | REGISTER + SIP/TCP, RTP/UDP | Every single-leg verb test; caller leg for multi-leg tests (`dial`, `conference`, `enqueue`) |
| **SIP UAS — `callee-uas`** (Stack B) | Same as A, second stack on a different ephemeral TCP port | REGISTER + SIP/TCP, RTP/UDP | Callee leg for multi-leg verb tests; idle on single-leg tests |
| **SIP UAC** (deferred) | Shares Stack A's transport | SIP/TCP, RTP/UDP | Reserved for outbound-origination tests — unlocks `alert`, `sip:decline`. Not wired into `tests/verbs/` yet |
| **HTTP server** | stdlib `net/http` ServeMux; `internal/webhook` | Serves `/hook`, `/status`, `/action/<verb>`, `/health` on loopback | Every webhook-driven (Phase-2) test |
| **WebSocket server** | `gorilla/websocket`; `internal/webhook/ws.go` | Session-routed `/ws/<session-id>`; text + binary frames | `listen` / `stream` today; future: AsyncAPI jambonz-WS, bidirectional `llm`/`agent` |
| **REST client** | stdlib `net/http`; `internal/provision` | HTTPS to `/v1/Calls`, `/Applications`, `/Accounts/...`, etc. | All tests that originate calls via `POST /Calls`; Tier 1–2 REST tests |
| **ngrok tunnel** | `ngrok/ngrok-go`; `internal/webhook/tunnel.go` | One public host carries both HTTPS (for HTTP server) and WSS (for WebSocket server) | All Phase-2 tests. Unset `NGROK_AUTHTOKEN` → Phase-2 tests skip cleanly |
| **Contract validator** | `santhosh-tekuri/jsonschema/v5`; `internal/contract` | Validates every inbound REST response and webhook body | Cross-cutting: runs inside the REST client and webhook server |
| **Deepgram STT** | `deepgram/deepgram-go-sdk/v3`; `internal/stt` | HTTPS to `api.deepgram.com` with linear16/8kHz | Post-call assertion for every audio-bearing test (say, play, gather_speech, dial, conference, enqueue, transcribe) |

### Jambonz-side components (the cluster we're testing)

| Component | What it does | We talk to it via |
|---|---|---|
| **drachtio SBC** | SIP edge — handles INVITE/ACK/BYE routing, RTP passthrough | Our UASes and UAC reach it on `sip.jambonz.me:5060` over TCP |
| **feature-server** | Executes verb scripts; invokes hooks | It calls us (through ngrok) for `call_hook`/`action_hook`/`call_status_hook` and for `listen`/`stream` WS audio |
| **api-server** | REST API | Our REST client POSTs `/Calls`, `/Applications`, etc. |
| **freeswitch** | Media (mixer, bridge, DTMF, recording) | Indirect — feature-server drives it; we observe effects through SIP + RTP |

## Traffic summary

| Direction | Protocol | From → To |
|---|---|---|
| Harness REST client → api-server | **HTTPS** | Outbound only; no tunnel needed |
| Harness UAS/UAC ↔ SBC | **SIP over TCP + RTP over UDP** | Registered connection from our ephemeral ports to `sip.jambonz.me` |
| Feature-server → Harness HTTP hooks | **HTTPS** (via ngrok) | `call_hook`, `action_hook`, `call_status_hook`, verb-specific hooks |
| Feature-server → Harness WS | **WSS** (via ngrok) | `listen`/`stream` binary audio + JSON metadata; future AsyncAPI |
| Harness STT helper → Deepgram | **HTTPS** | Post-call transcript check |

## Stack assignment per test shape

- **Single-leg verb test** (say, play, hangup, …): Stack A UAS only.
- **Multi-leg verb test** (dial, conference, enqueue/dequeue): Stack A UAS as caller, Stack B UAS as callee, jambonz bridges.
- **UAC origination test** (alert, sip:decline — *deferred*): Stack A UAC dials a jambonz-served URI; no Stack B.
- **Phase-2 webhook test** (gather, tag, redirect, config, dub, sip:*, listen, stream, transcribe, conference, enqueue, leave, message): ngrok tunnel is mandatory; Stack A (+ optionally B) involved depending on whether the verb is multi-leg.
