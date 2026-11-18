package mirror

import (
	"context"
	"errors"

	"github.com/majorcontext/motte/client"
)

const catchUpPage = 1000

// catchUp reads the events after the stored cursor until a page has none. A cursor that motte cannot serve is a Reset.
func (e *Engine) catchUp(ctx context.Context) error {
	c := e.motte.operator()
	for {
		svc, err := e.store.Service(ctx)
		if err != nil {
			return err
		}
		evs, cursor, err := c.Events(ctx, svc.EventsCursor, catchUpPage)
		if errors.Is(err, client.ErrEventsPruned) || errors.Is(err, client.ErrInvalidBody) {
			e.log.Warn("event cursor refused, resetting")
			return e.reset(ctx)
		}
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			e.notify("catchup", "")
			return nil
		}
		hits := make([]Hit, len(evs))
		var names []string
		for i, ev := range evs {
			hits[i] = e.hit(ev)
			names = append(names, hits[i].Mirrors...)
		}
		if err := e.store.RecordPage(ctx, hits, cursor, e.clock.Now()); err != nil {
			return err
		}
		e.wakeMirrors(names)
	}
}
