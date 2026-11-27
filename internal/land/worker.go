package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"

	"github.com/majorcontext/ward/wardapi"

	"github.com/majorcontext/ward/internal/check"
)

// errCheckEnd marks the span of a check wait that ended in a failed check or a store error.
var errCheckEnd = errors.New("land: the checks did not pass")

// maxRetries is the number of times that a land goes back to the first step.
const maxRetries = 8

type kind int

const (
	proceed kind = iota
	aborted
	retry
	failed
	landed
)

// outcome is the end of one pass over the steps of a land.
type outcome struct {
	kind      kind
	err       *Error
	retryCode wardapi.Code
	// keepCall marks a land call that ended with no definite answer of motte. The retry repeats the stored call with the same
	// id, because motte may have done it, and it does not count toward maxRetries.
	keepCall  bool
	storeErr  error
	landed    api.LandResponse
	branchGen int64
}

// callPause is the time between two sends of the same land call that had no definite answer.
const callPause = 2 * time.Second

// pause waits callPause on the clock of the service. It returns false when the service stops first.
func (s *Service) pause(ctx context.Context) bool {
	select {
	case <-s.clock.After(callPause):
		return true
	case <-ctx.Done():
		return false
	}
}

// noAnswer reports whether err leaves open that motte did the land call: vcs_land_timeout, a passed deadline, a failure of
// transport, or a 5xx answer. A local fault and any other 4xx answer are not in the class, so they end as internal_error.
func noAnswer(err error) bool {
	var wire *client.Error
	if errors.Is(err, client.ErrLandTimeout) {
		return true
	}
	if errors.As(err, &wire) {
		return wire.Status >= 500
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var opErr *net.OpError
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &opErr)
}

// storeFailure ends the attempt with no change to the land, so that a restart or the next land of the ref resumes it.
func (s *Service) storeFailure(err error) outcome {
	if s.ctx.Err() != nil {
		return outcome{kind: aborted}
	}
	return outcome{kind: aborted, storeErr: err}
}

// refusal maps an error of motte to the end of the attempt.
func (s *Service) refusal(err error) outcome {
	switch {
	case s.ctx.Err() != nil:
		return outcome{kind: aborted}
	case errors.Is(err, client.ErrLandConflict):
		paths := client.ConflictPaths(err)
		if paths == nil {
			paths = []string{}
		}
		return outcome{kind: failed, err: &Error{Code: CodeConflict, Message: "the branch changes paths that changed on the ref", Details: map[string]any{"paths": paths}}}
	case errors.Is(err, client.ErrLandInvalid), errors.Is(err, ErrNoMergeBase), errors.Is(err, client.ErrNotFound), errors.Is(err, client.ErrObjectTooLarge):
		return outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: "the branch cannot land on the ref"}}
	case errors.Is(err, client.ErrLandStale):
		return outcome{kind: retry, retryCode: CodeStale}
	case errors.Is(err, client.ErrLandTimeout), errors.Is(err, context.DeadlineExceeded):
		return outcome{kind: retry, retryCode: CodeTimeout}
	case errors.Is(err, check.ErrNoRunner):
		return outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: "the ref has required checks and the repository has no runner"}}
	}
	s.log.Error("land", "error", err.Error())
	return outcome{kind: failed, err: &Error{Code: CodeInternal, Message: "internal error"}}
}

// judge waits until the checks have a verdict for the tree of each member, in order, up to the first member that has a fail.
// It sends a request for each pending check, and it records the verdicts after each read. It returns the number of members
// that passed. A fail ends the wait only after every member before it has passed, and its outcome is the end of the member
// at that index.
func (s *Service) judge(ctx context.Context, repo string, ms []*member) (pass int, out outcome) {
	polls := 0
	asked := map[string]bool{}
	ctx, end := span(ctx, "ward.land.checks", attribute.Int("trees", len(ms)))
	defer func() {
		var err error
		if out.kind == failed || out.storeErr != nil {
			err = errCheckEnd
		}
		end(err, attribute.Int("polls", polls), attribute.Int("passed", pass), attribute.Bool("aborted", out.kind == aborted))
	}()
	for {
		polls++
		changed := s.checks.Changed()
		pending, failure, failAt := false, outcome{}, -1
		for i, m := range ms {
			dirty, waits := false, false
			for ci, c := range m.checks {
				v, err := s.checks.Verdict(ctx, repo, m.item.Tree, c, m.l.Mark)
				if err != nil {
					return 0, s.storeFailure(err)
				}
				dirty = dirty || m.l.Checks[ci].State != v.State
				m.l.Checks[ci].State = v.State
				switch {
				case v.State == check.Fail && failAt < 0:
					failAt = i
					failure = outcome{kind: failed, err: &Error{Code: CodeCheckFailed, Message: fmt.Sprintf("check %q failed", c.Name),
						Details: map[string]any{"check": c.Name, "summary": v.Result.Summary, "log_url": v.Result.LogURL}}}
				case v.State == check.Pending && failAt < 0:
					waits = true
					key := m.item.Tree + " " + c.Hash()
					if asked[key] {
						continue
					}
					asked[key] = true
					if err := s.checks.Ensure(ctx, repo, m.item.Tree, m.item.Tip, m.from, m.fromTree, c); err != nil {
						return 0, s.storeFailure(err)
					}
				}
			}
			if dirty {
				if err := s.save(m.l, m.l.State); err != nil {
					return 0, s.storeFailure(err)
				}
			}
			if failAt >= 0 {
				break
			}
			pending = pending || waits
		}
		switch {
		case failAt >= 0 && !pending:
			return failAt, failure
		case failAt < 0 && !pending:
			return len(ms), outcome{}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, outcome{kind: aborted}
		}
	}
}

// land is step 5. It stores the call before it sends it, so that a restart repeats the same call.
func (s *Service) land(ctx context.Context, merger *client.Client, l *Land, base, head string, branchGen int64) outcome {
	l.Attempt++
	l.Call = &Call{Base: base, Head: head, Tree: l.Tree, BranchGen: branchGen}
	if err := s.save(l, Landing); err != nil {
		return s.storeFailure(err)
	}
	return s.send(ctx, merger, l)
}

// send is the land call of motte. Motte moves the ref only when the tree of the land is the tree that the checks judged,
// and it answers a repeat of the same id with the first result.
func (s *Service) send(ctx context.Context, merger *client.Client, l *Land) outcome {
	c := l.Call
	ctx, end := span(ctx, "ward.land.send", attribute.Int("attempt", l.Attempt))
	res, err := merger.Land(ctx, l.Repo, api.LandRequest{ID: fmt.Sprintf("%s-%d", l.ID, l.Attempt), Ref: l.Ref, Base: c.Base, Head: c.Head, ExpectTree: c.Tree})
	end(err)
	if err != nil && s.ctx.Err() == nil && noAnswer(err) {
		return outcome{kind: retry, retryCode: CodeTimeout, keepCall: true}
	}
	if err != nil {
		return s.refusal(err)
	}
	return outcome{kind: landed, landed: res, branchGen: c.BranchGen}
}

// save stores the land in state st. It wakes the readers of the land when the state changed.
func (s *Service) save(l *Land, st State) error {
	changed := l.State != st
	l.State = st
	if err := s.store.SaveLand(s.ctx, *l); err != nil {
		return err
	}
	if changed {
		s.notify()
	}
	return nil
}

// finish deletes the branch of a landed land, and records the end.
func (s *Service) finish(l *Land, out outcome) error {
	ctx := s.ctx
	merger := s.client(grant.ScopeMerge, l.Repo, l.Branch)
	if out.kind == landed {
		l.Tip, l.Generation, l.FastForward, l.Commits = out.landed.Tip, out.landed.Generation, out.landed.FastForward, out.landed.Commits
		if l.Commits == nil {
			l.Commits = []api.LandedCommit{}
		}
		l.BranchDeleted = l.DeleteBranch && merger.DeleteRef(ctx, l.Branch, out.branchGen) == nil
		return s.save(l, Landed)
	}
	l.Error = out.err
	return s.save(l, Failed)
}
