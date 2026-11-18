package mirror

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"
)

// worker acts on the dirty refs of one mirror, one batch at a time.
type worker struct {
	e      *Engine
	en     Entry
	wakeCh chan struct{}

	mu        sync.Mutex
	reconcile bool
	seed      bool
	retryAt   time.Time
	next      time.Time

	reconcileFails int
	gen            int
}

func newWorker(e *Engine, en Entry) *worker {
	return &worker{e: e, en: en, wakeCh: make(chan struct{}, 1)}
}

func (w *worker) wake() {
	select {
	case w.wakeCh <- struct{}{}:
	default:
	}
}

func (w *worker) requestSeed() {
	w.mu.Lock()
	w.seed = true
	w.mu.Unlock()
	w.wake()
}

// seeded runs a wanted seed. It returns false when the worker must stop acting: the seed was refused or ctx ended.
// A failed seed retries with the backoff of a failed action.
func (w *worker) seeded(ctx context.Context) bool {
	for failures := 0; ctx.Err() == nil; {
		w.mu.Lock()
		want := w.seed
		w.mu.Unlock()
		if !want {
			return true
		}
		err := w.e.seed(ctx, w.en)
		switch {
		case err == nil:
			w.mu.Lock()
			w.seed = false
			w.mu.Unlock()
			w.requestReconcile()
			return true
		case errors.Is(err, errSeedRefused):
			w.e.log.Warn("seed refused", "mirror", w.en.Name, "error", err.Error())
			return false
		case ctx.Err() != nil:
			return false
		}
		failures++
		w.e.log.Warn("seed failed", "mirror", w.en.Name, "attempt", failures, "error", sanitize(err))
		select {
		case <-ctx.Done():
		case <-w.e.clock.After(backoff(failures)):
		}
	}
	return false
}

func (w *worker) requestReconcile() {
	w.mu.Lock()
	w.reconcile = true
	w.gen++
	w.retryAt = time.Time{}
	w.mu.Unlock()
	w.wake()
}

// reconcileDue reports whether a Reconcile is wanted and its retry wait is over.
func (w *worker) reconcileDue(now time.Time) (bool, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reconcile && !now.Before(w.retryAt), w.gen
}

// reconcileDone ends a pass that started at generation gen. A request that came during the pass keeps the flag.
func (w *worker) reconcileDone(err error, now time.Time, gen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.reconcileFails = 0
		if w.gen == gen {
			w.reconcile, w.retryAt = false, time.Time{}
		}
		return
	}
	w.reconcileFails++
	w.retryAt = now.Add(backoff(w.reconcileFails))
}

func (w *worker) maybeReconcile(ctx context.Context) {
	now := w.e.clock.Now()
	due, gen := w.reconcileDue(now)
	if !due {
		return
	}
	err := w.e.reconcile(ctx, w.en)
	if err != nil && ctx.Err() == nil {
		w.e.log.Warn("reconcile failed", "mirror", w.en.Name, "error", sanitize(err))
	}
	w.reconcileDone(err, now, gen)
}

func (w *worker) run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		if !w.seeded(ctx) {
			<-ctx.Done()
			return
		}
		w.maybeReconcile(ctx)
		done, err := w.step(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			failures++
			w.e.log.Warn("mirror worker step failed", "mirror", w.en.Name, "attempt", failures, "error", sanitize(err))
			select {
			case <-ctx.Done():
				return
			case <-w.e.clock.After(backoff(failures)):
			}
		case done:
			failures = 0
		default:
			failures = 0
			w.wait(ctx)
		}
	}
}

// step runs one batch of due refs. It reports whether it did work.
func (w *worker) step(ctx context.Context) (bool, error) {
	now := w.e.clock.Now()
	due, next, err := w.e.store.Due(ctx, w.en.Name, now)
	if err != nil {
		return false, err
	}
	due = w.e.dropExcluded(ctx, w.en, due)
	if len(due) > 0 {
		w.e.batch(ctx, w.en, due)
		return true, nil
	}
	w.next = next
	return false, nil
}

func (w *worker) wait(ctx context.Context) {
	at := w.next
	w.mu.Lock()
	if w.reconcile && (at.IsZero() || w.retryAt.Before(at)) {
		at = w.retryAt
	}
	w.mu.Unlock()
	var timer <-chan time.Time
	if !at.IsZero() {
		timer = w.e.clock.After(at.Sub(w.e.clock.Now()))
	}
	select {
	case <-ctx.Done():
	case <-w.wakeCh:
	case <-timer:
	}
}

var urlRE = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// sanitize returns the text of err without a URL, which could hold user information or a token.
func sanitize(err error) string {
	if err == nil {
		return ""
	}
	var ge *gitError
	if errors.As(err, &ge) {
		return failureText[ClassLocal]
	}
	return urlRE.ReplaceAllString(err.Error(), "<url>")
}
