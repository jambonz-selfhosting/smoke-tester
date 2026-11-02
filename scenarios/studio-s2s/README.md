# Studio speech-to-speech vendor scenarios

One scenario per vendor for the "speech-to-speech vendors, batch two" work (agent-studio
`docs/plan/S2S-VENDORS-DESIGN.md` §6). Each needs a published Studio agent on that vendor, built from the same brief:

- prompt: an order-status desk; greets, answers order questions, transfers billing questions, hangs up when the caller is done;
- an HTTP tool `get_order_status` (order id in, a status such as "shipped yesterday" out);
- a transfer for billing that returns to the agent;
- the hangup tool.

Set `agent_id`, then run:

```bash
go run ./cmd/studioscenarios -scenario scenarios/studio-s2s/xai.json -out /tmp/s2s -sid <cookie-file>
```

A scenario with `expect` exits non-zero and lists each unmet expectation; `result.json` keeps the Studio call record
and the failures. `no-credential.json` is run against an agent whose credential was deleted: the call must end with a
credential completion reason.
