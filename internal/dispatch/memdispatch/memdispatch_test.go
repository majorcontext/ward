package memdispatch_test

import (
	"context"
	"testing"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch/dispatchtest"
	"github.com/majorcontext/ward/internal/dispatch/memdispatch"
	"github.com/majorcontext/ward/runnerproto"
)

func TestConformance(t *testing.T) {
	dispatchtest.Run(t, dispatchtest.Impl{New: func(_ *testing.T, _ check.Store, repos []string) dispatchtest.Harness {
		d := memdispatch.New(repos...)
		return dispatchtest.Harness{Dispatcher: d, Receive: func(ctx context.Context, repo string) (runnerproto.Request, bool) { return d.Receive(ctx, repo) }}
	}})
}
