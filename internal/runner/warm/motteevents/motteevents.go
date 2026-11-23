// Package motteevents is a warm.Watcher that polls the ref events of motte. The route needs an operator grant, so the
// runner uses this watcher only when it has one. The poll dials out only.
package motteevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/runner/warm"
)

const (
	pageSize    = 1000
	cleanupTime = 10 * time.Second
)

// Watcher reports the repository of each ref event that follows its start. Events that precede the start are skipped:
// the periodic watcher and the fetch of a missing commit cover them. After retention removes events that the watcher
// has not read, it reports "".
type Watcher struct {
	Client *client.Client
	Poll   time.Duration
	Clock  check.Clock

	mu      sync.Mutex
	orphans []string
}

var _ warm.Watcher = (*Watcher)(nil)

func (w *Watcher) Watch(ctx context.Context, changed func(repo string)) error {
	cursor, err := w.begin(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	for {
		select {
		case <-w.Clock.After(w.Poll):
		case <-ctx.Done():
			return nil
		}
		if cursor, err = w.drain(ctx, cursor, changed); errors.Is(err, client.ErrEventsPruned) {
			changed("")
			cursor, err = w.begin(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// begin returns the cursor of the newest event. It reads the history to its end. When retention has removed the start of
// the history, that read fails for good, so begin takes the cursor of a subscription that it deletes at once. A delete
// that fails is kept and tried again by the next begin.
func (w *Watcher) begin(ctx context.Context) (string, error) {
	cursor, err := w.drain(ctx, "", func(string) {})
	if !errors.Is(err, client.ErrEventsPruned) {
		return cursor, err
	}
	var secret [16]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	w.deleteOrphans(ctx)
	sub, err := w.Client.CreateSubscription(ctx, api.CreateSubscriptionRequest{URL: "http://127.0.0.1:9/", Secret: hex.EncodeToString(secret[:])})
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.orphans = append(w.orphans, sub.ID)
	w.mu.Unlock()
	w.deleteOrphans(ctx)
	return sub.Cursor, nil
}

// deleteOrphans deletes the subscriptions that begin made and has not yet deleted. It runs to the end when ctx has ended,
// within a time limit. A delete that fails stays on the list for the next call.
func (w *Watcher) deleteOrphans(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTime)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	var left []string
	for _, id := range w.orphans {
		if err := w.Client.DeleteSubscription(ctx, id); err != nil && !errors.Is(err, client.ErrNotFound) {
			left = append(left, id)
		}
	}
	w.orphans = left
}

// drain reads events until the route has no more, passes each repository to report, and returns the new cursor.
func (w *Watcher) drain(ctx context.Context, cursor string, report func(repo string)) (string, error) {
	for {
		evs, next, err := w.Client.Events(ctx, cursor, pageSize)
		if err != nil {
			return cursor, err
		}
		for _, e := range evs {
			report(e.Repo)
		}
		cursor = next
		if len(evs) == 0 {
			return cursor, nil
		}
	}
}
