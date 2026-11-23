package motteevents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/ward/internal/runner/warm/motteevents"
	"github.com/majorcontext/ward/internal/runner/warm/warmtest"
)

func TestConformance(t *testing.T) {
	warmtest.Run(t, warmtest.Impl{New: func(t *testing.T, clock *warmtest.Clock) warmtest.Harness {
		var mu sync.Mutex
		var events []api.Event
		started := make(chan struct{})
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			from, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
			_ = json.NewEncoder(w).Encode(api.Events{Events: events[from:], Cursor: strconv.Itoa(len(events))})
			once.Do(func() { close(started) })
		}))
		t.Cleanup(srv.Close)
		c := client.New(srv.URL, client.StaticGrant("g"))
		return warmtest.Harness{
			Watcher: &motteevents.Watcher{Client: c, Poll: 1, Clock: clock},
			Repo:    "native:r",
			Change: func() {
				<-started
				mu.Lock()
				events = append(events, api.Event{Repo: "native:r", Ref: "refs/heads/main"})
				mu.Unlock()
			},
		}
	}})
}

func TestFailedSubscriptionDeleteIsRetried(t *testing.T) {
	var mu sync.Mutex
	var created, deleteTries int
	deleted := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(api.ErrorBody{Error: api.Error{Code: api.CodeEventsPruned}})
		case r.Method == http.MethodPost:
			created++
			_ = json.NewEncoder(w).Encode(api.Subscription{ID: "s" + strconv.Itoa(created), Cursor: "0"})
		default:
			deleteTries++
			if deleteTries == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			deleted <- r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	clock := &warmtest.Clock{}
	w := &motteevents.Watcher{Client: client.New(srv.URL, client.StaticGrant("g")), Poll: 1, Clock: clock}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Watch(ctx, func(string) {}) }()
	clock.Fire()
	select {
	case got := <-deleted:
		if !strings.HasSuffix(got, "/s1") {
			t.Errorf("the first delete that succeeded was %q, want the retry of s1", got)
		}
	case err := <-done:
		t.Fatalf("Watch returned %v before it retried the delete", err)
	}
	cancel()
	<-done
}
