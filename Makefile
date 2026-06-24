.PHONY: help build test test-rest test-sip test-verbs test-one test-report lint clean deps

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
	@echo "  make test-one NAME=TestVerb_Conference_TwoParty [PKG=./tests/verbs/]"
	@echo "                    # run a single test by name (anchored ^NAME$$)"
	@echo "  make test-report  # run all tests, write self-contained report.html"
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

# Run a single test by name. The -run pattern is anchored (^NAME$) so
# NAME=TestVerb_Conference_TwoParty does not also match siblings like
# TestVerb_Conference_TwoParty_Muted. PKG defaults to ./tests/... so you
# don't have to know which package the test lives in; pass PKG to narrow
# it and speed up compilation. -v surfaces the per-step markers and the
# === FAILURE SUMMARY === block.
#
#   make test-one NAME=TestVerb_Conference_TwoParty
#   make test-one NAME=TestVerb_Conference_TwoParty PKG=./tests/verbs/
# Run a single test by typing its name as the make goal:
#
#   make TestVerb_Conference_TwoParty
#
# It searches all test packages, picks the one that defines the test, and
# runs only that package (so an unrelated package's suite setup can't fail
# the run). Anchored ^NAME$ so siblings don't also match.
test-one:
ifndef NAME
	$(error NAME is required, e.g. make test-one NAME=TestVerb_Conference_TwoParty)
endif
	@pkg=$$(grep -rl "func $(NAME)(" --include='*_test.go' tests | head -1 | xargs -I{} dirname {}); \
	test -n "$$pkg" || { echo "test $(NAME) not found"; exit 1; }; \
	echo "running $(NAME) in ./$$pkg/"; \
	go test -count=1 -timeout 300s -v -run '^$(NAME)$$' "./$$pkg/"

# Any goal that looks like a Go test name (TestXxx) runs that single test.
Test%:
	@$(MAKE) --no-print-directory test-one NAME=$@

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
