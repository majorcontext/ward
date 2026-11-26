// Package fakeremote is a gitremote.Remote that keeps the refs of each URL in memory. It reads no object, so a test that uses it
// needs no git repository on the remote side.
package fakeremote

import (
	"context"
	"sync"

	"github.com/majorcontext/ward/internal/gitremote"
)

// Remote holds the refs of each URL.
type Remote struct {
	mu   sync.Mutex
	refs map[string]map[string]string
}

var _ gitremote.Remote = (*Remote)(nil)

// New returns a remote with no ref.
func New() *Remote { return &Remote{refs: map[string]map[string]string{}} }

func (r *Remote) List(_ context.Context, url string, _ gitremote.Cred) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.refs[url] {
		out[k] = v
	}
	return out, nil
}

func (r *Remote) Push(_ context.Context, _, url string, _ gitremote.Cred, specs []gitremote.PushSpec) (map[string]gitremote.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs[url] == nil {
		r.refs[url] = map[string]string{}
	}
	refs, out := r.refs[url], map[string]gitremote.Outcome{}
	for _, s := range specs {
		if s.LeaseSet && refs[s.Ref] != s.Lease {
			out[s.Ref] = gitremote.Stale
			continue
		}
		if s.Commit == "" {
			delete(refs, s.Ref)
		} else {
			refs[s.Ref] = s.Commit
		}
		out[s.Ref] = gitremote.OK
	}
	return out, nil
}
