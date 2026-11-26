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

func candidateRef(id string) string { return "refs/ward/candidates/" + id }

// run takes one land to its end. A cancelled context leaves the land as it is, and a restart resumes it.
// It reports whether a check failed the land. It returns an error only when the end of the land could not be recorded.
func (s *Service) run(ctx context.Context, l Land) (bool, error) {
	for {
		out := s.attempt(ctx, &l)
		if out.kind == retry && out.keepCall {
			if !s.pause(ctx) {
				return false, nil
			}
			continue
		}
		if out.kind == retry {
			if l.Retries >= maxRetries {
				out = outcome{kind: failed, err: &Error{Code: out.retryCode, Message: "the ref kept moving or motte kept timing out"}}
			} else {
				l.Retries++
				l.Call = nil
				continue
			}
		}
		if out.kind == aborted {
			return false, out.storeErr
		}
		return out.kind == failed && out.err.Code == CodeCheckFailed, s.finish(&l, out)
	}
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

// attempt runs steps 1 to 5.
func (s *Service) attempt(ctx context.Context, l *Land) outcome {
	reader, merger := s.client("read", l.Repo), s.client("merge", l.Repo)
	if l.State == Landing && l.Call != nil {
		return s.send(ctx, merger, l)
	}
	pctx, end := span(ctx, "ward.land.prepare")
	branch, err := reader.Ref(pctx, l.Branch)
	if err != nil {
		end(err)
		return s.refusal(err)
	}
	tip, err := reader.Ref(pctx, l.Ref)
	if err != nil {
		end(err)
		return s.refusal(err)
	}
	base, err := s.baseOf(pctx, reader, l, tip.Commit, branch.Commit)
	if err != nil {
		end(err)
		return s.refusal(err)
	}
	candidate, tree, err := s.candidate(pctx, reader, merger, l, tip.Commit, base, branch.Commit)
	end(err, attribute.Bool("fast_forward", base == tip.Commit))
	if err != nil {
		return s.refusal(err)
	}
	if out := s.check(ctx, reader, l, tip.Commit, candidate, tree); out.kind != proceed {
		return out
	}
	return s.land(ctx, merger, l, base, branch.Commit, branch.Generation)
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

// candidate returns the commit and the tree that the ref will hold, as far as motte can tell before the land.
func (s *Service) candidate(ctx context.Context, reader, merger *client.Client, l *Land, tip, base, head string) (string, string, error) {
	if base == tip {
		c, err := reader.Commit(ctx, head)
		return head, c.Tree, err
	}
	name := candidateRef(l.ID)
	if old, err := reader.Ref(ctx, name); err == nil {
		if err := merger.DeleteRef(ctx, name, old.Generation); err != nil {
			return "", "", err
		}
	}
	if _, err := merger.SwapRef(ctx, name, 0, tip); err != nil {
		return "", "", err
	}
	out, err := merger.Land(ctx, l.Repo, api.LandRequest{Ref: name, Base: base, Head: head})
	if err != nil {
		return "", "", err
	}
	c, err := reader.Commit(ctx, out.Tip)
	return out.Tip, c.Tree, err
}

// check is step 4: it reads the check file at the tip, asks for the checks that lack a pass, and waits for the results.
func (s *Service) check(ctx context.Context, reader *client.Client, l *Land, tip, candidate, tree string) outcome {
	checks, err := requiredChecks(ctx, reader, tip)
	var fileErr fileError
	switch {
	case errors.As(err, &fileErr):
		return outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: fileErr.Error()}}
	case err != nil:
		return s.refusal(err)
	}
	if len(checks) > 0 {
		if _, ok := s.checks.Runner(l.Repo); !ok {
			return s.refusal(check.ErrNoRunner)
		}
	}
	if l.State != Checking || l.Tree != tree {
		if l.Mark, err = s.checks.Mark(ctx); err != nil {
			return s.storeFailure(err)
		}
	}
	states := make([]CheckState, len(checks))
	for i, c := range checks {
		states[i] = CheckState{Name: c.Name, State: check.Pending}
	}
	l.Tree, l.Checks = tree, states
	if err := s.save(l, Checking); err != nil {
		return s.storeFailure(err)
	}
	return s.await(ctx, l.Repo, tree, candidate, l.Mark, checks, func(vs []check.Verdict) error {
		for i, v := range vs {
			l.Checks[i].State = v.State
		}
		return s.save(l, l.State)
	})
}

// await waits until every check has a verdict for tree and returns the first fail. It sends a request for each pending
// check, and it calls report with the verdicts after each read.
func (s *Service) await(ctx context.Context, repo, tree, candidate string, mark int64, checks []check.Check, report func([]check.Verdict) error) (out outcome) {
	polls := 0
	ctx, end := span(ctx, "ward.land.checks", attribute.Int("checks", len(checks)))
	defer func() {
		var err error
		if out.kind == failed || out.storeErr != nil {
			err = errCheckEnd
		}
		end(err, attribute.Int("polls", polls), attribute.Bool("failed", out.kind == failed), attribute.Bool("aborted", out.kind == aborted))
	}()
	for {
		polls++
		changed := s.checks.Changed()
		verdicts := make([]check.Verdict, len(checks))
		pending := false
		for i, c := range checks {
			v, err := s.checks.Verdict(ctx, repo, tree, c, mark)
			if err != nil {
				return s.storeFailure(err)
			}
			verdicts[i] = v
			if v.State == check.Pending {
				pending = true
				if err := s.checks.Ensure(ctx, repo, tree, candidate, c); err != nil {
					return s.storeFailure(err)
				}
			}
		}
		if err := report(verdicts); err != nil {
			return s.storeFailure(err)
		}
		for i, v := range verdicts {
			if v.State == check.Fail {
				return outcome{kind: failed, err: &Error{Code: CodeCheckFailed, Message: fmt.Sprintf("check %q failed", checks[i].Name),
					Details: map[string]any{"check": checks[i].Name, "summary": v.Result.Summary, "log_url": v.Result.LogURL}}}
			}
		}
		if !pending {
			return outcome{}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return outcome{kind: aborted}
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

// finish is step 6: it removes the candidate ref, deletes the branch of a landed land, and records the end.
func (s *Service) finish(l *Land, out outcome) error {
	ctx := s.ctx
	reader, merger := s.client("read", l.Repo), s.client("merge", l.Repo)
	name := candidateRef(l.ID)
	if ref, err := reader.Ref(ctx, name); err == nil {
		_ = merger.DeleteRef(ctx, name, ref.Generation)
	}
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
