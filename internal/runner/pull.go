package runner

import (
	"context"
	"errors"
	"time"

	"github.com/majorcontext/ward/runnerproto"
)

const (
	pollRetryMax = 30 * time.Second
	renewDivisor = 3
)

// Pull polls ward for leases until ctx ends. It holds at most Concurrency leases. It dials out only, so the runner needs
// no inbound network. Each lease runs one check, and the runner renews the lease until the check ends.
func (r *Runner) Pull(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		select {
		case r.slot <- struct{}{}:
		case <-ctx.Done():
			return
		}
		lease, err := r.poll(ctx)
		if err != nil || lease == nil {
			<-r.slot
			if err == nil {
				failures = 0
				continue
			}
			failures++
			r.o.Log.Warn("lease poll failed", "attempt", failures, "error", err.Error())
			select {
			case <-r.o.Clock.After(min(time.Second<<min(failures, 5), pollRetryMax)):
			case <-ctx.Done():
			}
			continue
		}
		failures = 0
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer func() { <-r.slot }()
			r.runLease(ctx, *lease)
		}()
	}
}

func (r *Runner) poll(ctx context.Context) (*runnerproto.Lease, error) {
	tok, err := r.o.RunnerToken(r.o.Clock.Now().Add(tokenLife))
	if err != nil {
		return nil, err
	}
	return runnerproto.PollLease(ctx, r.o.HTTP, r.o.WardURL, tok, r.o.Isolation)
}

// runLease runs the check of a lease and renews the lease meanwhile. It cancels the check when ward ends the lease.
func (r *Runner) runLease(ctx context.Context, l runnerproto.Lease) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		every := time.Duration(l.TTLSeconds) * time.Second / renewDivisor
		for {
			select {
			case <-r.o.Clock.After(every):
			case <-runCtx.Done():
				return
			}
			tok, err := r.o.RunnerToken(r.o.Clock.Now().Add(tokenLife))
			if err == nil {
				_, err = runnerproto.RenewLease(runCtx, r.o.HTTP, r.o.WardURL, tok, l.LeaseID)
			}
			var se *runnerproto.StatusError
			if errors.As(err, &se) && se.Final() {
				r.o.Log.Warn("lease lost", "request_id", l.Request.RequestID, "status", se.Code)
				cancel()
				return
			}
			if err != nil && runCtx.Err() == nil {
				r.o.Log.Warn("lease not renewed", "request_id", l.Request.RequestID, "error", err.Error())
			}
		}
	}()
	r.execute(runCtx, l.Request)
	cancel()
	<-renewed
}
