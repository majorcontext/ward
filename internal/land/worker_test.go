package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/motte/client"
)

type queueStore struct {
	Store
	nexts atomic.Int64
}

func (q *queueStore) NextLands(context.Context, Key, int) ([]Land, error) {
	if q.nexts.Add(1) <= 2 {
		return nil, errors.New("disk full")
	}
	return nil, nil
}

func (q *queueStore) UnfinishedKeys(context.Context) ([]Key, error) { return []Key{{"r", "main"}}, nil }

func TestWorkerRetriesAStoreErrorWithBackoffAndSweepRestartsALostQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		st := &queueStore{}
		s := New(Config{}, st, nil, nil, nil)
		ctx, cancel := context.WithCancel(t.Context())
		_ = s.Start(ctx)
		for i, wait := range []time.Duration{3 * storeBackoffMin, sweepEvery} {
			time.Sleep(wait)
			synctest.Wait()
			if got := st.nexts.Load(); got != int64(i+3) {
				t.Fatalf("NextLands calls = %d, want %d: retry after 1 s and 2 s, then restart the queue at the sweep", got, i+3)
			}
		}
		cancel()
		s.Wait()
	})
}

func TestNoAnswerClass(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"land timeout", client.ErrLandTimeout, true},
		{"deadline", fmt.Errorf("x: %w", context.DeadlineExceeded), true},
		{"eof", io.ErrUnexpectedEOF, true},
		{"dial", &url.Error{Op: "Post", Err: &net.OpError{Op: "dial"}}, true},
		{"url local", &url.Error{Op: "Post", Err: errors.New("unsupported scheme")}, false},
		{"5xx", &client.Error{Status: 502}, true},
		{"4xx", &client.Error{Status: 403}, false},
		{"stale", fmt.Errorf("x: %w", client.ErrLandStale), false},
		{"local", errors.New("disk full"), false},
	} {
		if got := noAnswer(c.err); got != c.want {
			t.Errorf("%s: noAnswer = %v, want %v", c.name, got, c.want)
		}
	}
}
