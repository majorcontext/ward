// Package memdispatch is the in-memory implementation of dispatch.Dispatcher for tests. It keeps one queue for each repository.
package memdispatch

import (
	"context"

	"github.com/majorcontext/ward/internal/dispatch"
	"github.com/majorcontext/ward/runnerproto"
)

// Dispatcher queues requests in memory.
type Dispatcher struct {
	queues map[string]chan runnerproto.Request
}

var _ dispatch.Dispatcher = (*Dispatcher)(nil)

// New returns a dispatcher with a queue for each of repos.
func New(repos ...string) *Dispatcher {
	d := &Dispatcher{queues: map[string]chan runnerproto.Request{}}
	for _, r := range repos {
		d.queues[r] = make(chan runnerproto.Request, 64)
	}
	return d
}

func (d *Dispatcher) Dispatch(ctx context.Context, req runnerproto.Request) error {
	q, ok := d.queues[req.Repo]
	if !ok {
		return dispatch.ErrNoRunner
	}
	select {
	case q <- req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Receive returns the next request of repo. It returns false when ctx ends first.
func (d *Dispatcher) Receive(ctx context.Context, repo string) (runnerproto.Request, bool) {
	select {
	case r := <-d.queues[repo]:
		return r, true
	case <-ctx.Done():
		return runnerproto.Request{}, false
	}
}
