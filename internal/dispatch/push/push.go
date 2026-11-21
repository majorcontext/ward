// Package push dispatches a check request by posting it to the hook URL of the runner of its repository.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/majorcontext/ward/internal/check"
	"github.com/majorcontext/ward/internal/dispatch"
	"github.com/majorcontext/ward/runnerproto"
)

const postTimeout = 30 * time.Second

// Dispatcher posts each request to the runner of its repository.
type Dispatcher struct {
	runners map[string]check.Runner
	http    *http.Client
}

var _ dispatch.Dispatcher = (*Dispatcher)(nil)

// New returns a dispatcher for the push runners in runners.
func New(runners []check.Runner) *Dispatcher {
	d := &Dispatcher{runners: map[string]check.Runner{}, http: &http.Client{Timeout: postTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, r := range runners {
		if !r.Pull() {
			d.runners[r.Repo] = r
		}
	}
	return d
}

func (d *Dispatcher) Dispatch(ctx context.Context, req runnerproto.Request) error {
	r, ok := d.runners[req.Repo]
	if !ok {
		return dispatch.ErrNoRunner
	}
	secret, err := r.Secret()
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(runnerproto.SignatureHeader, runnerproto.Sign(secret, body))
	resp, err := d.http.Do(hr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("runner answered %d", resp.StatusCode)
	}
	return nil
}
