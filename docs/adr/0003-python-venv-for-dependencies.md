# 3. Python with plain venv for dependency management

- **Status:** Superseded by [ADR-0011](0011-go-modules-for-dependencies.md)
- **Date:** 2026-04-18
- **Superseded:** 2026-04-18
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** tooling, python, superseded

> **Superseded on 2026-04-18.** The project switched from Python to Go after a
> 1-day spike ([spikes/001-sipgo-diago](../../spikes/001-sipgo-diago/)) showed
> sipgo+diago delivers full signaling+media control with no native build step,
> no SWIG, and materially better ergonomics than pjsua2. See
> [ADR-0011](0011-go-modules-for-dependencies.md) for the replacement dependency-
> management decision and [ADR-0013](0013-sipgo-diago-for-sip-rtp.md) for the SIP
> stack. Record below is kept for history.

## Context

The test harness needs SIP (pjsua2), an HTTP server (FastAPI), and an HTTP client (httpx). Python is the pragmatic choice — pjsua2 has mature Python bindings, FastAPI/httpx are ubiquitous, and contributors to jambonz already read Python. The language is settled; the question is dependency management.

The maintainer explicitly wants to be able to **wipe and recreate the environment at any time** without fighting tooling.

## Decision

Use the Python standard library's `venv` module for environment isolation and `pip` + `requirements.txt` for dependency installation.

- Virtual environment lives at `.venv/` in the repo root and is `.gitignore`d.
- Runtime deps are pinned in `requirements.txt`; dev-only deps in `requirements-dev.txt`.
- `pyproject.toml` may exist for packaging metadata, but **not** for dependency management.
- The Makefile exposes `make venv`, `make install`, `make clean` to make the wipe-and-recreate workflow one command.
- No lockfile tooling (`uv.lock`, `poetry.lock`, `Pipfile.lock`) unless reproducibility problems emerge in practice.

Full environment reset is: `make clean && make venv && make install`.

## Consequences

- Positive: `rm -rf .venv` is a complete reset with no tool-specific caches to hunt down.
- Positive: any Python 3 install on any Debian / macOS box can bootstrap the repo — no extra tool required.
- Positive: low cognitive load for casual contributors.
- Negative: no lockfile means dependency resolution is not bit-for-bit reproducible across machines. Mitigation: pin exact versions (`==`) in `requirements.txt`.
- Negative: slower installs than `uv`. Acceptable for a release-gate suite that installs infrequently.
- Follow-up: `requirements.txt` should pin with `==`, not `>=`, and be regenerated via `pip freeze` on intentional upgrades.

## Alternatives considered

### Option A — `uv`
Rejected: the maintainer wants a tool-free "wipe anytime" workflow. `uv` adds a binary dependency and a lockfile format; its speed is not needed for a release-gate run.

### Option B — Poetry
Rejected: heavier than needed, opinionated project structure, `pyproject.toml`-driven resolution that complicates the "nuke the venv" story.

### Option C — Conda
Rejected: overkill for pure-Python deps, conflicts with system Python on Debian/EC2 targets.

### Option D — System Python + `pip install --user`
Rejected: pollutes the system, can't be cleanly wiped, creates permission headaches on Debian.

## References

- ADR-0005 (pjsua2) — pjsua2 is a native-built dep, so the venv + `scripts/build_pjsua2.sh` is where its Python binding is installed.
