package main

import (
	"io"
	"strings"
	"testing"
)

func TestParentWhoseSubtestsAllSkipIsNotALeaf(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"skip","Package":"example.com/p","Test":"TestA/one","Elapsed":0}`,
		`{"Action":"skip","Package":"example.com/p","Test":"TestA/two","Elapsed":0}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestA","Elapsed":99}`,
		`{"Action":"pass","Package":"example.com/p","Test":"TestB","Elapsed":99}`,
	}, "\n")
	rep := consume(strings.NewReader(stream), io.Discard)
	b := &budget{leaf: 10, pkgBase: 10, pkgs: map[string]float64{}}
	over := rep.check(b, "")
	if len(over) != 1 || !strings.Contains(over[0], "TestB") {
		t.Fatalf("over = %q, want only TestB", over)
	}
}

func TestPackageFailurePrintsTheOutputOfARunningTest(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"output","Package":"example.com/p","Test":"TestHang","Output":"panic: test timed out\n"}`,
		`{"Action":"fail","Package":"example.com/p","Elapsed":600}`,
	}, "\n")
	var out strings.Builder
	rep := consume(strings.NewReader(stream), &out)
	if !rep.failed || !strings.Contains(out.String(), "panic: test timed out") {
		t.Fatalf("failed = %v, output = %q, want the held trace printed", rep.failed, out.String())
	}
}
