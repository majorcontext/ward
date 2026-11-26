package memory

import (
	"context"
	"slices"
	"time"

	"github.com/majorcontext/ward/internal/check"
)

func (s *Store) keyHasPass(r check.Request) bool {
	for _, res := range s.results {
		if res.Repo == r.Repo && res.Tree == r.Tree && res.Hash == r.Hash && res.Env == r.Env && res.Status == check.StatusPass {
			return true
		}
	}
	return false
}

func (s *Store) findRequest(id string) *check.Request {
	for _, r := range s.requests {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *Store) insertRequest(r check.Request) {
	r = clone(r)
	r.Open = true
	s.requests = append(s.requests, &r)
}

func (s *Store) OpenRequest(_ context.Context, r check.Request) (check.Request, bool, error) {
	defer s.lock()()
	for _, cur := range s.requests {
		if cur.Open && cur.Repo == r.Repo && cur.Tree == r.Tree && cur.Hash == r.Hash && cur.Env == r.Env {
			return clone(*cur), false, nil
		}
	}
	if s.keyHasPass(r) {
		return r, false, nil
	}
	s.insertRequest(r)
	return r, true, nil
}

func (s *Store) Request(_ context.Context, id string) (check.Request, bool, error) {
	defer s.lock()()
	if r := s.findRequest(id); r != nil {
		return clone(*r), true, nil
	}
	return check.Request{}, false, nil
}

func (s *Store) OpenRequests(context.Context) ([]check.Request, error) {
	defer s.lock()()
	var out []check.Request
	for _, r := range s.requests {
		if r.Open {
			out = append(out, clone(*r))
		}
	}
	return out, nil
}

func (s *Store) Supersede(_ context.Context, old string, next check.Request, now time.Time) (bool, error) {
	defer s.lock()()
	cur := s.findRequest(old)
	if cur == nil {
		return false, nil
	}
	if !cur.Open || s.keyHasPass(*cur) {
		return false, nil
	}
	var until time.Time
	for _, l := range s.leases {
		if l.request == old && l.until.After(now) && l.until.After(until) {
			until = l.until
		}
	}
	if !until.IsZero() {
		return false, &check.LeasedError{Until: until}
	}
	cur.Open = false
	s.insertRequest(next)
	return true, nil
}

func (s *Store) AddResult(_ context.Context, r check.Result) (bool, error) {
	defer s.lock()()
	for _, have := range s.results {
		if have.RequestID == r.RequestID {
			return false, nil
		}
	}
	r.ID = int64(len(s.results) + 1)
	s.results = append(s.results, r)
	if cur := s.findRequest(r.RequestID); cur != nil {
		cur.Open = false
	}
	if r.Status == check.StatusPass {
		for _, cur := range s.requests {
			if cur.Repo == r.Repo && cur.Tree == r.Tree && cur.Hash == r.Hash && cur.Env == r.Env {
				cur.Open = false
			}
		}
	}
	return true, nil
}

func (s *Store) Results(_ context.Context, repo, tree, hash, env string) ([]check.Result, error) {
	defer s.lock()()
	var out []check.Result
	for _, r := range s.results {
		if r.Repo == repo && r.Tree == tree && r.Hash == hash && r.Env == env {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) MaxResultID(context.Context) (int64, error) {
	defer s.lock()()
	return int64(len(s.results)), nil
}

func (s *Store) liveLease(request string, now time.Time) bool {
	for _, l := range s.leases {
		if l.request == request && l.until.After(now) {
			return true
		}
	}
	return false
}

func (s *Store) Lease(_ context.Context, repos []string, kid, leaseID string, now, until time.Time) (check.Request, bool, error) {
	defer s.lock()()
	for _, r := range s.requests {
		if !r.Open || !slices.Contains(repos, r.Repo) || s.liveLease(r.ID, now) {
			continue
		}
		s.leases = append(s.leases, lease{id: leaseID, request: r.ID, kid: kid, until: until})
		r.AcceptedAt = now
		return clone(*r), true, nil
	}
	return check.Request{}, false, nil
}

func (s *Store) RenewLease(_ context.Context, leaseID, kid string, repos []string, until time.Time) (bool, error) {
	defer s.lock()()
	for i, l := range s.leases {
		if l.id != leaseID || l.kid != kid {
			continue
		}
		r := s.findRequest(l.request)
		if r == nil || !r.Open || !slices.Contains(repos, r.Repo) {
			return false, nil
		}
		for _, n := range s.leases[i+1:] {
			if n.request == l.request {
				return false, nil
			}
		}
		s.leases[i].until = until
		return true, nil
	}
	return false, nil
}

func (s *Store) Leased(_ context.Context, requestID, kid string) (bool, error) {
	defer s.lock()()
	for _, l := range s.leases {
		if l.request == requestID && l.kid == kid {
			return true, nil
		}
	}
	return false, nil
}
