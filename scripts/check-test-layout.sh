#!/usr/bin/env bash
# Suite/detail layout (ADR-0017): <feature>_test.go holds at most MAX suite
# tests; <feature>_detail_test.go starts with `//go:build detail` + a blank
# line; *_unit_test.go holds offline unit tests and is never capped.
set -u

MAX=${MAX:-2}
fail=0
err() { echo "test-layout: $*" >&2; fail=1; }

for f in tests/rest/*_test.go tests/verbs/*_test.go; do
  tagged=0
  grep -q '^//go:build' "$f" && tagged=1
  case "$f" in
    *_detail_test.go)
      [ "$(sed -n 1p "$f")" = '//go:build detail' ] && [ -z "$(sed -n 2p "$f")" ] \
        || err "$f must start with '//go:build detail' followed by a blank line" ;;
    *)
      [ $tagged -eq 0 ] || err "$f has a //go:build line; detail tests belong in *_detail_test.go"
      case "$f" in *_unit_test.go) continue ;; esac
      n=$(grep -cE '^func Test[A-Za-z0-9_]+\([A-Za-z0-9_]+ \*testing\.T\)' "$f")
      [ "$n" -le "$MAX" ] || err "$f has $n suite tests (max $MAX); move the rest to ${f%_test.go}_detail_test.go" ;;
  esac
done

[ $fail -eq 0 ] && echo "test-layout: OK"
exit $fail
