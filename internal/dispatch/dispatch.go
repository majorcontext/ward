// Package dispatch is the seam between the check manager and the runners: how an open check request reaches a runner.
package dispatch

import (
	"context"
	"errors"

	"github.com/majorcontext/ward/runnerproto"
)

// ErrNoRunner means that no runner of this kind serves the repository of the request.
var ErrNoRunner = errors.New("check: no runner for the repository")

// Dispatcher gets an open request to a runner of its repository. A push implementation posts it to the runner. A pull
// implementation queues it until a runner leases it. Dispatch returns nil when a runner of req.Repo will receive req, and an
// error when it cannot say so; the manager repeats the call with a backoff. The manager stores the request as open before the
// call. Delivery is at least once with the same request_id, so a runner may see a request again and runs it once. A request
// for a repository with no runner of this kind is an ErrNoRunner.
type Dispatcher interface {
	Dispatch(ctx context.Context, req runnerproto.Request) error
}
