.PHONY: help build test test-rest test-sip test-verbs test-srtp test-report test-drachtio test-drachtio-uas test-drachtio-uac test-drachtio-uac-keepalive test-drachtio-none list-drachtio lint clean deps

# Parallelism: default to min(NumCPU, 4). Go's `go test -parallel N`
# controls how many t.Parallel() tests run concurrently within a package.
#
# The limiting resource is NOT the dev box — it's the jambonz CLUSTER under
# test plus the third-party services each call drives (Deepgram STT+TTS
# websockets, ngrok tunnel). A media-heavy verb test (agent/conference/dial/
# listen) holds a live RTP leg + 1-2 vendor websockets for its whole run.
# Measured on a 4-vCPU cluster (mediajam media server): at -parallel 8 the
# full suite flakes 5-7/49 each run — STT drops words/times out, DTMF is
# late, fork audio comes back silent — and the failing SET changes run to
# run (the signature of concurrent-load saturation, not a logic bug). The
# same tests pass individually and in small groups. At -parallel 4 the full
# suite is stable (≤1-2 residual LLM-content/correlation flakes). So 4 is
# the gate default; it tracks the cluster's real concurrent-call headroom,
# not the dev box core count.
#
# Override on the command line for a beefier cluster:
#   make test PARALLEL=8
# Or to debug serially:
#   make test PARALLEL=1
NUM_CPU := $(shell sysctl -n hw.ncpu 2>/dev/null || nproc 2>/dev/null || echo 4)
PARALLEL ?= $(shell echo $$(( $(NUM_CPU) < 4 ? $(NUM_CPU) : 4 )))

help:
	@echo "smoke-tester — release-gate harness"
	@echo
	@echo "  make deps         # go mod tidy"
	@echo "  make build        # compile all packages"
	@echo "  make test         # go test ./...  (parallel=$(PARALLEL))"
	@echo "  make test-rest    # Tier 1/2 REST tests only"
	@echo "  make test-sip     # Tier 3+ SIP tests only"
	@echo "  make test-verbs   # per-verb tests (outbound calls via app_json)"
	@echo "  make TestVerb_Conference_TwoParty"
	@echo "                    # run a single test by name (anchored ^NAME$$)"
	@echo "  make builtin_hangup_test.go"
	@echo "                    # run every TestXxx defined in one _test.go file"
	@echo "  make test-report  # run all tests, write self-contained report.html"
	@echo "  make test-drachtio"
	@echo "                    # run-all: every drachtio test (drachtio build tag)"
	@echo "                    # NOTE: the generic 'make Test%' pattern rule does"
	@echo "                    # NOT work for these — it omits -tags drachtio."
	@echo "  make test-drachtio RUN=TestDrachtio_SessionTimer_UASRefresher"
	@echo "                    # run-one: exact test name (RUN is a regexp, so"
	@echo "                    # an exact name matches just that one test)"
	@echo "  make test-drachtio RUN=TestDrachtio_SessionTimer"
	@echo "                    # run-a-file's-tests: RUN as a prefix regexp runs"
	@echo "                    # every test in one file. Convention: each"
	@echo "                    # tests/drachtio/<area>_test.go file's tests all"
	@echo "                    # share one TestDrachtio_<Area>_* prefix (e.g."
	@echo "                    # session_timer_test.go -> TestDrachtio_SessionTimer),"
	@echo "                    # so the prefix IS the file selector — no"
	@echo "                    # separate FILE= flag needed or provided."
	@echo "  make list-drachtio"
	@echo "                    # list every available drachtio test name, so"
	@echo "                    # you don't have to grep the source to find one"
	@echo "  make test-drachtio-uas   # shortcut: only the UAS-refresher (proactive re-INVITE) test"
	@echo "  make test-drachtio-uac   # shortcut: only the UAC-expiry (timeout BYE) test"
	@echo "  make test-drachtio-uac-keepalive  # shortcut: only the UAC-keepalive (proactive refresh, call stays up) test"
	@echo "  make test-drachtio-none  # shortcut: only the default-refresher=none (no timer armed, call survives) test"
	@echo "  make lint         # go vet ./..."
	@echo "  make clean        # remove build artifacts"
	@echo
	@echo "Override parallelism: make test PARALLEL=4"
	@echo "Detected CPUs: $(NUM_CPU); using -parallel $(PARALLEL)"

deps:
	go mod tidy

build:
	go build ./...

# TEST_PACKAGES is the explicit list of packages with tests. We don't
# use `./...` because that would walk every package and emit a noisy
# `[no test files]` line for each one. `make build` already verifies
# the non-test packages compile; here we only want to run real tests.
TEST_PACKAGES := ./tests/... ./internal/contract/...

# Per-package timeouts. Sized to ~2× the observed parallel runtime, so
# one wedged test can hang past its in-test watchdog without the suite
# binary getting nuked at Go's 10-minute alarm. The per-test
# WithTimeout() is the real circuit breaker — these are just upper
# bounds on the binary lifetime.
#
#   verbs: ~80-90s observed parallel → 180s
#   rest:  ~22s   observed parallel → 60s
#   all:   verbs + rest serial      → 300s
test:
	go test -count=1 -timeout 300s -parallel $(PARALLEL) $(TEST_PACKAGES)

test-rest:
	go test -count=1 -timeout 60s -parallel $(PARALLEL) ./tests/rest/...

test-sip:
	go test -count=1 -timeout 180s -parallel $(PARALLEL) ./tests/sip/...

test-verbs:
	go test -count=1 -timeout 180s -parallel $(PARALLEL) ./tests/verbs/...

# Manual, opt-in SRTP/TLS send-out test (dial verb srtpEncryption over a
# sips: URI). EXCLUDED from `make test` via the `manual_srtp` build tag; it
# dials an EXTERNAL destination you control so you can packet-capture the
# outbound leg and confirm sips:/TLS + SRTP (a=crypto). Requires
# JAMBONZ_IT_SIP_TLS_DEST_DOMAIN and JAMBONZ_IT_SIP_TLS_DEST_DOMAIN_PORT
# (skips when unset).
#
#   make test-srtp \
#     JAMBONZ_IT_SIP_TLS_DEST_DOMAIN=your-capture-host.example.com \
#     JAMBONZ_IT_SIP_TLS_DEST_DOMAIN_PORT=5061
test-srtp:
	go test -tags manual_srtp -count=1 -timeout 120s -v -run "^TestVerb_Dial_Sip_SRTP_TLS$$" ./tests/verbs/

# Run a single test by typing its name as the make goal:
#
#   make TestVerb_Conference_TwoParty
#
# It searches all test packages, picks the one that defines the test, and runs
# only that package (so an unrelated package's suite setup can't fail the run).
# Anchored ^NAME$ so siblings like ..._Muted don't also match. -v surfaces the
# per-step markers and the === FAILURE SUMMARY === block.
Test%:
	@name=$@; \
	pkg=$$(grep -rl "func $$name(" --include='*_test.go' tests | head -1 | xargs -I{} dirname {}); \
	test -n "$$pkg" || { echo "test $$name not found"; exit 1; }; \
	echo "running $$name in ./$$pkg/"; \
	go test -count=1 -timeout 300s -v -run "^$$name$$" "./$$pkg/"

# Run every test defined in ONE _test.go file by typing the file as the goal:
#
#   make builtin_hangup_test.go
#   make handoff_test.go PARALLEL=2
#
# You can pass the bare filename (it's located under tests/) or a path.
# Go compiles per-package, not per-file, so we can't hand `go test` a single
# file and get its package linked — instead we scrape the top-level
# `func TestXxx(` names out of the file, OR them into one anchored -run pattern
# (^(A|B|C)$), and run that against the file's own package (the rest of the
# package still compiles + links, but only this file's tests execute). Each
# alternative is individually anchored so a sibling whose name is a prefix of
# one of ours can't sneak in.
#
# The FORCE prereq defeats make's "target file already exists → up to date"
# short-circuit: when you pass a real path (tests/verbs/foo_test.go), that file
# is on disk, so without FORCE make would say "up to date" and skip the recipe.
%_test.go: FORCE
	@file="$@"; \
	test -f "$$file" || file=$$(find tests -name "$@" | head -1); \
	test -n "$$file" -a -f "$$file" || { echo "file not found: $@"; exit 1; }; \
	pkg=$$(dirname "$$file"); \
	tests=$$(grep -oE '^func (Test[A-Za-z0-9_]+)\(' "$$file" | sed -E 's/^func //; s/\($$//'); \
	test -n "$$tests" || { echo "no top-level TestXxx funcs in $$file"; exit 1; }; \
	pattern=$$(echo "$$tests" | paste -sd '|' -); \
	echo "running $$(echo "$$tests" | wc -l | tr -d ' ') test(s) from $$file in ./$$pkg/"; \
	echo "  -run '^($$pattern)$$'"; \
	go test -count=1 -timeout 300s -v -parallel $(PARALLEL) -run "^($$pattern)$$" "./$$pkg/"

# RFC 4028 session-timer tests, exercised against a live sbc-inbound
# (drachtio). Long-running (~90-120s per test), so they are EXCLUDED from
# `make test` / `make test-report` via the `drachtio` build tag — without
# `-tags drachtio` these files compile to an empty package (see doc.go) and
# TEST_PACKAGES' `./tests/...` walk finds nothing to run here. They also
# depend on server-side drachtio default-refresher configuration, so they
# aren't safe to run unconditionally in the default suite.
#
# Future tests: this package is future-proofed generically, not per-test —
# no Makefile edit is needed to add a new drachtio test:
#   - run-all:            make test-drachtio
#   - run-one-test:       make test-drachtio RUN=TestDrachtio_SessionTimer_UASRefresher
#   - run-one-file's-tests: RUN doubles as a per-file selector under one
#     convention: one file = one TestDrachtio_<Area>_* prefix (e.g. all
#     tests in session_timer_test.go start with TestDrachtio_SessionTimer).
#     `make test-drachtio RUN=TestDrachtio_SessionTimer` runs that whole
#     file's tests without a separate FILE= flag. `go test` has no
#     "run only this file" flag (it compiles per-package, not per-file), so
#     a true FILE= target would either be a no-op or require re-deriving
#     this exact same prefix from the filename — the naming convention
#     already does that job, so keep new drachtio test files following it
#     and skip a FILE= target as unnecessary indirection.
#   - discover names:     make list-drachtio
RUN ?= .
test-drachtio:
	go test -tags drachtio -count=1 -timeout 360s -parallel 2 -v -run "$(RUN)" ./tests/drachtio/

# Memorable shortcuts for the two session-timer tests, so you don't have to
# remember RUN= or the exact TestDrachtio_SessionTimer_* name.
test-drachtio-uas:
	go test -tags drachtio -count=1 -timeout 360s -parallel 2 -v -run "TestDrachtio_SessionTimer_UASRefresher" ./tests/drachtio/

test-drachtio-uac:
	go test -tags drachtio -count=1 -timeout 360s -parallel 2 -v -run "TestDrachtio_SessionTimer_UACRefresherExpiry" ./tests/drachtio/

test-drachtio-uac-keepalive:
	go test -tags drachtio -count=1 -timeout 360s -parallel 2 -v -run "TestDrachtio_SessionTimer_UACRefresherKeepalive" ./tests/drachtio/

test-drachtio-none:
	go test -tags drachtio -count=1 -timeout 360s -parallel 2 -v -run "TestDrachtio_SessionTimer_NoneNoTimer" ./tests/drachtio/

# List every available drachtio test name (requires -tags drachtio, same
# reason as above: without it the package is empty). Use this to discover
# what to pass to RUN= instead of grepping the source.
list-drachtio:
	go test -tags drachtio -list '.*' ./tests/drachtio/

test-report:
	@# `go test -json` streams NDJSON test events; cmd/testreport renders
	@# them into a self-contained HTML file. Don't fail make on test
	@# failures — the point is to produce a viewable report even when red.
	go test -json -count=1 -timeout 300s -parallel $(PARALLEL) $(TEST_PACKAGES) | go run ./cmd/testreport > report.html || true
	@echo "wrote report.html (open it in your browser)"

lint:
	go vet ./...

clean:
	rm -rf bin/ coverage.out report.xml report.html
	find . -name '*.wav' -not -path './spikes/*' -not -path './tests/verbs/testdata/*' -delete

# Empty phony prerequisite used to force pattern-rule recipes (%_test.go) to
# always run even when a real file of that name exists on disk.
.PHONY: FORCE
FORCE:
