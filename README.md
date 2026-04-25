# smoke-tester

Black-box integration tests for the open-source [jambonz](https://www.jambonz.org) voice platform. Drives a real cluster from the outside via REST + SIP + RTP, with content-level audio assertions via Deepgram STT.

## What it's for

1. **Release gate** — run the suite before tagging a jambonz release. If anything goes red, don't ship.
2. **Post-deploy verification** — after upgrading or redeploying a cluster, run the suite to confirm no regressions in REST, SIP signalling, media bridging, TTS/STT, or webhook delivery.
3. **Synthetic monitoring** — schedule the suite (cron / k8s CronJob / your monitoring tool) to continuously prove the cluster handles real call flows. Failures page on-call before customers notice.

The harness provisions everything it needs (SIP users via `/Clients`, Deepgram credential, ngrok webhook tunnel) per run and tears it down — no manual setup beyond `.env`.

## Install

**Prerequisites:** Go 1.26+ and `make`.

```bash
# macOS
brew install go

# Debian / Ubuntu
sudo apt-get install -y golang-go make

# RHEL / Fedora
sudo dnf install -y golang make

# Verify
go version   # >= 1.26
```

## Run

```bash
git clone https://github.com/jambonz-selfhosting/smoke-tester.git
cd smoke-tester
make deps                      # go mod tidy
cp .env.example .env
$EDITOR .env                   # fill in credentials (see below)
make test                      # full release-gate run, ~90s parallel
```

**Required credentials in `.env`:**

| Variable | What it's for |
|---|---|
| `JAMBONZ_API_URL` | REST base (e.g. `https://jambonz.me/api/v1`) |
| `JAMBONZ_SIP_DOMAIN` | SIP endpoint (e.g. `sip.jambonz.me`) |
| `JAMBONZ_API_KEY` + `JAMBONZ_ACCOUNT_SID` | Account scope — for verb tests, /Clients, /SpeechCredentials |
| `JAMBONZ_SP_API_KEY` + `JAMBONZ_SP_SID` | Service-provider scope — for REST tests |
| `NGROK_AUTHTOKEN` | Webhook tunnel for Phase-2 verbs and call-status callbacks |
| `DEEPGRAM_API_KEY` | TTS + STT inside jambonz, plus offline transcript verification |

See [.env.example](.env.example) for the full annotated template.

## Targets

```bash
make test           # everything: REST + verbs + contract layer (~90s)
make test-rest      # REST CRUD only (~22s)
make test-verbs     # call-flow verb tests only (~80s)
make test-report    # write self-contained report.html
make help           # all targets, current parallelism setting
```

Parallelism auto-scales to `min(NumCPU, 8)`. Override with `make test PARALLEL=4`.

## What you'll see

Live progress every 5s (no need for `-v`):

```
[heartbeat 45s] running=8 done=11 (11 pass, 0 fail) | now: TestVerb_Conference_TwoParty[step:place-listener-call]@5s, TestVerb_Dial_User_Bridge[step:assert-bridge-audio-transcript]@10s, ...
```

On failure, an end-of-run summary names the test, step, and reason — no log grepping:

```
=== FAILURE SUMMARY (1) ===
  FAIL TestVerb_Gather_Speech [step:assert-transcript-sun-shining] transcript "the rain is falling" missing "sun"
============================
```

## Synthetic monitoring

For continuous monitoring, schedule a recurring run and alert on non-zero exit:

```bash
# crontab — every 5 minutes
*/5 * * * * cd /opt/smoke-tester && make test 2>&1 | tee /var/log/smoke-tester.log; \
            test ${PIPESTATUS[0]} -eq 0 || /usr/local/bin/page-oncall
```

The suite is idempotent — every resource it creates is name-prefixed `it-<runID>-` and tagged for cleanup. Concurrent runs against the same cluster are safe.

## More

- **[CLAUDE.md](CLAUDE.md)** — orientation for contributors and AI sessions
- **[HANDOFF.md](HANDOFF.md)** — current state, recent work, open questions
- **[docs/adr/](docs/adr/)** — architecture decision records (every significant choice has one)
- **[docs/coverage-matrix.md](docs/coverage-matrix.md)** — what's tested today, what's planned
- **[docs/architecture/components.md](docs/architecture/components.md)** — system diagram
