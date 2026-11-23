// Package periodic is the fallback warm.Watcher: it reports every known repository as changed at a fixed interval.
package periodic

import (
	"context"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/runner/warm"
)

// Watcher reports each repository of Repos every Every.
type Watcher struct {
	Repos func() []string
	Every time.Duration
	Clock check.Clock
}

var _ warm.Watcher = Watcher{}

func (w Watcher) Watch(ctx context.Context, changed func(repo string)) error {
	for {
		select {
		case <-w.Clock.After(w.Every):
		case <-ctx.Done():
			return nil
		}
		for _, r := range w.Repos() {
			changed(r)
		}
	}
}
