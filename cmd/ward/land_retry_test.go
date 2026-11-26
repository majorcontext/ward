package main

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"
)

type step func(*http.Request) (*http.Response, error)

func (f step) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(code int, body string) step {
	return func(*http.Request) (*http.Response, error) {
		var r io.Reader = strings.NewReader(body)
		if body == "cut" {
			r = iotest.ErrReader(io.ErrUnexpectedEOF)
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(r)}, nil
	}
}
func drop(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }
func stall(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestLandCommandReadsAnAcceptedLandAgainAndNeverSendsAPostAgain(t *testing.T) {
	queued, landed := resp(202, `{"id":"L1","state":"queued"}`), resp(200, `{"id":"L1","state":"landed","tip":"abc123"}`)
	for name, c := range map[string]struct {
		post     step
		polls    []step
		exit     int
		requests int
		output   string
	}{
		"dropped poll":        {queued, []step{drop, drop, landed}, 0, 4, "tip abc123\n"},
		"bad gateway":         {queued, []step{resp(502, ""), resp(504, ""), landed}, 0, 4, "tip abc123\n"},
		"cut off answer":      {queued, []step{resp(200, "cut"), landed}, 0, 3, "tip abc123\n"},
		"cut off 401":         {queued, []step{resp(401, "cut"), landed}, 1, 2, "status 401"},
		"dropped POST":        {drop, []step{landed}, 1, 1, "the request failed"},
		"recovers at the end": {queued, append(slices.Repeat([]step{drop}, 16), landed), 0, 18, "tip abc123\n"},
		"poll that stalls":    {queued, []step{drop, stall}, 1, 0, "land L1"},
	} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			gitIn(t, work, "init", "-q", "-b", "b")
			gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "x")
			gitIn(t, work, "update-ref", "refs/remotes/origin/main", "HEAD")
			synctest.Test(t, func(t *testing.T) {
				n := 0
				old := noRedirect.Transport
				t.Cleanup(func() { noRedirect.Transport = old })
				noRedirect.Transport = step(func(r *http.Request) (*http.Response, error) {
					n++
					if r.Method == http.MethodGet && r.URL.Path != "/v1/lands/L1" {
						t.Errorf("read of %s, want the land that the POST made", r.URL.Path)
					}
					if r.Method == http.MethodPost {
						return c.post(r)
					}
					return c.polls[min(n-2, len(c.polls)-1)](r)
				})
				var out bytes.Buffer
				start := time.Now()
				code := runLand(t.Context(), []string{"-C", work, "native:r", "b"}, func(k string) string { return map[string]string{"WARD_URL": "http://ward.test", "WARD_TOKEN": "t"}[k] }, time.Now, &out, &out)
				if code != c.exit || (c.requests > 0 && n != c.requests) || !strings.Contains(out.String(), c.output) || time.Since(start) > landRetryWindow {
					t.Fatalf("exit %d after %d requests in %s\noutput: %s", code, n, time.Since(start), &out)
				}
			})
		})
	}
}
