package main

import (
	"strings"
	"testing"
)

func TestParse_PackageSetupFailureCountsAsFailure(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"output","Package":"x/rest","Output":"suite setup failed: missing properties\n"}`,
		`{"Action":"output","Package":"x/rest","Output":"FAIL\tx/rest\t0.9s\n"}`,
		`{"Action":"fail","Package":"x/rest","Elapsed":0.9}`,
		`{"Action":"run","Package":"x/unit","Test":"TestOK"}`,
		`{"Action":"pass","Package":"x/unit","Test":"TestOK","Elapsed":0.1}`,
		`{"Action":"pass","Package":"x/unit","Elapsed":0.2}`,
	}, "\n")
	sum, err := parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.Failed != 1 || sum.Passed != 1 || sum.Total != 2 {
		t.Fatalf("got failed=%d passed=%d total=%d, want 1/1/2", sum.Failed, sum.Passed, sum.Total)
	}
	f := sum.Failures[0]
	if f.Package != "x/rest" || f.Name != packageSetup || !strings.Contains(f.Output, "suite setup failed") {
		t.Fatalf("failure = %+v, want x/rest %s carrying the package output", f, packageSetup)
	}
}

func TestParse_PackageWithFailedTestGetsNoExtraRow(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"run","Package":"x/rest","Test":"TestBad"}`,
		`{"Action":"fail","Package":"x/rest","Test":"TestBad","Elapsed":0.1}`,
		`{"Action":"fail","Package":"x/rest","Elapsed":0.2}`,
	}, "\n")
	sum, err := parse(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sum.Failed != 1 || sum.Total != 1 || sum.Failures[0].Name != "TestBad" {
		t.Fatalf("got failed=%d total=%d failures=%+v, want only TestBad", sum.Failed, sum.Total, sum.Failures)
	}
}
