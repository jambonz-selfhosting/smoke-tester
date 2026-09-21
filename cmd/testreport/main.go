// testreport renders the NDJSON stream from `go test -json` into a single
// self-contained HTML file.
//
// It is written for a suite that makes real phone calls: when something goes
// red the useful artifact is the failing test's own step log, not a count. So
// failures are listed first, expanded, with their captured output; everything
// else collapses out of the way.
//
// Run:
//
//	go test -json ./... | go run ./cmd/testreport > report.html
//
// Exit status is 0 whenever a report was written, including for a red suite —
// the caller decides what a failure means. A malformed stream, or one carrying
// no test events at all, is an error: silently writing an empty page is how
// the previous version of this target hid its own breakage.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// event is one line of `go test -json`. Fields we don't use are omitted.
type event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

type result struct {
	Package string
	Name    string
	Status  string // pass | fail | skip
	Elapsed float64
	Output  string
}

// Failed reports whether this test should lead the report.
func (r result) Failed() bool { return r.Status == "fail" }

type summary struct {
	Generated               string
	Total                   int
	Passed, Failed, Skipped int
	Duration                string
	Failures                []result
	Rest                    []result
	PackagesNoTests         []string
}

func main() {
	sum, err := parse(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testreport: %v\n", err)
		os.Exit(1)
	}
	if err := page.Execute(os.Stdout, sum); err != nil {
		fmt.Fprintf(os.Stderr, "testreport: rendering report: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "testreport: %d tests, %d failed, %d skipped\n",
		sum.Total, sum.Failed, sum.Skipped)
}

func parse(r io.Reader) (summary, error) {
	var (
		sum     summary
		results = map[string]*result{}
		output  = map[string][]string{}
		noTests []string
		wall    float64
		lines   int
	)

	sc := bufio.NewScanner(r)
	// Step logs are long; the default 64KiB token limit truncates them.
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			// `go test` writes build errors as plain text on the same stream.
			if line != "" {
				noTests = append(noTests, line)
			}
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // a stray line must not cost us the whole report
		}
		lines++

		key := e.Package + "." + e.Test
		if e.Test == "" {
			// package-level event
			if e.Action == "output" && strings.Contains(e.Output, "no test files") {
				noTests = append(noTests, e.Package)
			}
			if e.Action == "pass" || e.Action == "fail" {
				wall += e.Elapsed
			}
			continue
		}

		switch e.Action {
		case "output":
			output[key] = append(output[key], e.Output)
		case "pass", "fail", "skip":
			results[key] = &result{
				Package: e.Package,
				Name:    e.Test,
				Status:  e.Action,
				Elapsed: e.Elapsed,
			}
		}
	}
	if err := sc.Err(); err != nil {
		return sum, fmt.Errorf("reading go test output: %w", err)
	}
	if lines == 0 {
		return sum, fmt.Errorf("no `go test -json` events on stdin " +
			"(is the pipeline wired up, and did the build succeed?)")
	}
	if len(results) == 0 {
		return sum, fmt.Errorf("stream carried %d events but no test results", lines)
	}

	for key, res := range results {
		res.Output = strings.Join(output[key], "")
		switch res.Status {
		case "pass":
			sum.Passed++
		case "fail":
			sum.Failed++
		case "skip":
			sum.Skipped++
		}
		if res.Failed() {
			sum.Failures = append(sum.Failures, *res)
		} else {
			sum.Rest = append(sum.Rest, *res)
		}
	}
	sum.Total = len(results)
	sum.Duration = time.Duration(wall * float64(time.Second)).Round(time.Second).String()
	sum.Generated = time.Now().Format(time.RFC1123)
	sum.PackagesNoTests = noTests

	byName := func(rs []result) {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].Package != rs[j].Package {
				return rs[i].Package < rs[j].Package
			}
			return rs[i].Name < rs[j].Name
		})
	}
	byName(sum.Failures)
	byName(sum.Rest)
	return sum, nil
}

var page = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>smoke-tester report</title>
<style>
 :root { color-scheme: light dark; }
 body { font: 14px/1.5 ui-sans-serif, system-ui, sans-serif; margin: 2rem auto; max-width: 70rem; padding: 0 1rem; }
 h1 { font-size: 1.4rem; margin-bottom: .25rem; }
 .meta { opacity: .7; font-size: .85rem; margin-bottom: 1.5rem; }
 .tiles { display: flex; gap: .75rem; flex-wrap: wrap; margin-bottom: 2rem; }
 .tile { border: 1px solid currentColor; border-radius: .5rem; padding: .6rem 1rem; min-width: 6rem; }
 .tile .n { font-size: 1.6rem; font-weight: 600; display: block; }
 .fail .n { color: #c0392b; } .pass .n { color: #1e8449; } .skip .n { opacity: .6; }
 details { border: 1px solid rgba(128,128,128,.4); border-radius: .4rem; padding: .5rem .75rem; margin-bottom: .5rem; }
 details[open] { background: rgba(128,128,128,.06); }
 summary { cursor: pointer; font-weight: 600; }
 summary .pkg { font-weight: 400; opacity: .6; font-size: .85rem; margin-left: .5rem; }
 summary .t { float: right; opacity: .6; font-weight: 400; }
 pre { overflow-x: auto; background: rgba(128,128,128,.12); padding: .6rem; border-radius: .3rem; white-space: pre-wrap; }
 h2 { font-size: 1.05rem; margin-top: 2rem; }
 .none { opacity: .6; }
</style></head><body>
<h1>smoke-tester report</h1>
<div class="meta">{{.Generated}} &middot; wall {{.Duration}}</div>

<div class="tiles">
 <div class="tile"><span class="n">{{.Total}}</span>tests</div>
 <div class="tile fail"><span class="n">{{.Failed}}</span>failed</div>
 <div class="tile pass"><span class="n">{{.Passed}}</span>passed</div>
 <div class="tile skip"><span class="n">{{.Skipped}}</span>skipped</div>
</div>

<h2>Failures</h2>
{{if .Failures}}
 {{range .Failures}}
 <details open>
  <summary>{{.Name}}<span class="pkg">{{.Package}}</span><span class="t">{{printf "%.2fs" .Elapsed}}</span></summary>
  <pre>{{.Output}}</pre>
 </details>
 {{end}}
{{else}}<p class="none">None.</p>{{end}}

<h2>Passed and skipped</h2>
{{if .Rest}}
 {{range .Rest}}
 <details>
  <summary>{{.Name}} <em>{{.Status}}</em><span class="pkg">{{.Package}}</span><span class="t">{{printf "%.2fs" .Elapsed}}</span></summary>
  <pre>{{.Output}}</pre>
 </details>
 {{end}}
{{else}}<p class="none">None.</p>{{end}}

{{if .PackagesNoTests}}
<h2>Notes</h2>
<pre>{{range .PackagesNoTests}}{{.}}
{{end}}</pre>
{{end}}
</body></html>
`))
