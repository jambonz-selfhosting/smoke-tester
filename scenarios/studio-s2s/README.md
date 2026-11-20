# Studio speech-to-speech vendor scenarios

One scenario per realtime voice API vendor, all on the same order-desk agent:

- prompt: an order-status desk; greets, answers order questions, transfers billing questions, hangs up when the caller is done;
- an HTTP tool `get_order_status` (order id in, "shipped yesterday" out);
- a transfer for billing to a number that never answers, so it returns to the agent;
- the hangup tool.

`kugel-fluid` is KugelAudio Fluid (`kugelaudio_s2s`). Studio records a GPT-Live protocol tool call as a system row,
not on the agent turn, so the order tool is checked by its "get_order_status ran" event instead of `tools_ok`; there is
no `agent_hangup`, since Fluid sometimes says goodbye without the hangup tool.

## Every vendor, automatically

```bash
make test-studio                         # every vendor with a key in .env
make test-studio RUN='^TestStudio_S2S$/nova' # one vendor
```

`tests/studio` builds everything itself and deletes it at the end: the vendor credential (on the jambonz account the
Studio workspace is connected to, under a run-unique label), the prompt, the tool when the workspace has none, and one
agent per vendor. A vendor without a key in `.env` passes without a call. `TestStudio_S2S_CredentialDeleted` deletes
an agent's credential and expects Studio to refuse the test with `CRED_NOT_FOUND`. Set `STUDIO_*` in `.env` (see
`.env.example`). The suite carries the `studio` build tag, so `make test` and `make test-report` never run it.

## One existing agent, by hand

Set `agent_id` in a copy of the scenario, then:

```bash
go run ./cmd/studioscenarios -scenario my-xai.json -out /tmp/s2s -sid <cookie-file>
```

A scenario with `expect` exits non-zero and lists each unmet expectation; `result.json` keeps the Studio call record
and the failures.
