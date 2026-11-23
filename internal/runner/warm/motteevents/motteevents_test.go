package motteevents_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			Watcher: motteevents.Watcher{Client: c, Poll: 1, Clock: clock},
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
