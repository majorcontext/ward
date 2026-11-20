package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type failTransport struct{ used bool }

func (f *failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.used = true
	return nil, errors.New("network used")
}

func writeChecks(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".ward"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ward", "checks.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A laptop run must never reach a server: a pass there would stand for a land.
func TestLocalCheckRunsInOrderExitsOnFailureAndUsesNoNetwork(t *testing.T) {
	ft := &failTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = ft
	t.Cleanup(func() { http.DefaultTransport = old })
	dir := writeChecks(t, `{"checks":[
		{"name":"ok","run":["sh","-c","echo hello-from-ok"]},
		{"name":"bad","run":["sh","-c","echo oops >&2; exit 3"]},
		{"name":"after","run":["true"]},
	]}`)

	var out, errb bytes.Buffer
	if code := runLocalCheck(t.Context(), []string{"--dir", dir}, time.Now, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out.String())
	}
	s := out.String()
	sum := s[strings.Index(s, "== summary"):]
	iOK, iBad, iAfter := strings.Index(sum, "pass  ok"), strings.Index(sum, "fail  bad"), strings.Index(sum, "pass  after")
	if iOK < 0 || iBad < iOK || iAfter < iBad {
		t.Fatalf("summary out of order or incomplete:\n%s", sum)
	}
	if !strings.Contains(s, "hello-from-ok") || !strings.Contains(errb.String(), "oops") {
		t.Fatalf("output not streamed: %q %q", s, errb.String())
	}
	if ft.used {
		t.Fatal("ward check used the network")
	}

	out.Reset()
	if code := runLocalCheck(t.Context(), []string{"--dir", dir, "--only", "ok"}, time.Now, &out, &errb); code != 0 || strings.Contains(out.String(), "bad") {
		t.Fatalf("--only ok: exit %d\n%s", code, out.String())
	}
	if code := runLocalCheck(t.Context(), []string{"--dir", dir, "--only", "nope"}, time.Now, &out, &errb); code != 2 {
		t.Fatalf("unknown --only: exit %d, want 2", code)
	}
	if code := runLocalCheck(t.Context(), []string{"--dir", t.TempDir()}, time.Now, &out, &errb); code != 2 {
		t.Fatalf("missing file: exit %d, want 2", code)
	}
}
