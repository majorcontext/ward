package land

import (
	"context"
	"errors"
	"fmt"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/check"
)

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
	retryCode string
	landed    api.LandResponse
	branchGen int64
}

func candidateRef(id string) string { return "refs/ward/candidates/" + id }

// run takes one land to its end. A cancelled context leaves the land as it is, and a restart resumes it.
// It returns an error only when the end of the land could not be recorded.
func (s *Service) run(l Land) error {
	for {
		out := s.attempt(&l)
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
			return nil
		}
		return s.finish(&l, out)
	}
}

// attempt runs steps 1 to 5.
func (s *Service) attempt(l *Land) outcome {
	ctx := s.ctx
	reader, merger := s.client("read", l.Repo), s.client("merge", l.Repo)
	if l.State == Landing && l.Call != nil {
		return s.send(ctx, merger, l)
	}
	branch, err := reader.Ref(ctx, l.Branch)
	if err != nil {
		return s.refusal(err)
	}
	tip, err := reader.Ref(ctx, l.Ref)
	if err != nil {
		return s.refusal(err)
	}
	base, err := mergeBase(ctx, reader, tip.Commit, branch.Commit)
	if err != nil {
		return s.refusal(err)
	}
	candidate, tree, err := s.candidate(ctx, reader, merger, l, tip.Commit, base, branch.Commit)
	if err != nil {
		return s.refusal(err)
	}
	if out := s.check(ctx, reader, l, tip.Commit, candidate, tree); out.kind != proceed {
		return out
	}
	return s.land(ctx, merger, l, base, branch.Commit, branch.Generation)
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
	case errors.Is(err, client.ErrLandTimeout):
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
			return s.refusal(err)
		}
	}
	states := make([]CheckState, len(checks))
	for i, c := range checks {
		states[i] = CheckState{Name: c.Name, State: check.Pending}
	}
	l.Tree, l.Checks = tree, states
	if err := s.save(l, Checking); err != nil {
		return s.refusal(err)
	}
	return s.await(ctx, l, checks, candidate)
}

func (s *Service) await(ctx context.Context, l *Land, checks []check.Check, candidate string) outcome {
	for {
		changed := s.checks.Changed()
		verdicts := make([]check.Verdict, len(checks))
		pending := false
		for i, c := range checks {
			v, err := s.checks.Verdict(ctx, l.Repo, l.Tree, c, l.Mark)
			if err != nil {
				return s.refusal(err)
			}
			verdicts[i] = v
			l.Checks[i].State = v.State
			if v.State == check.Pending {
				pending = true
				if err := s.checks.Ensure(ctx, l.Repo, l.Tree, candidate, c); err != nil {
					return s.refusal(err)
				}
			}
		}
		if err := s.save(l, l.State); err != nil {
			return s.refusal(err)
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
		return s.refusal(err)
	}
	return s.send(ctx, merger, l)
}

// send is the land call of motte. Motte moves the ref only when the tree of the land is the tree that the checks judged,
// and it answers a repeat of the same id with the first result.
func (s *Service) send(ctx context.Context, merger *client.Client, l *Land) outcome {
	c := l.Call
	res, err := merger.Land(ctx, l.Repo, api.LandRequest{ID: fmt.Sprintf("%s-%d", l.ID, l.Attempt), Ref: l.Ref, Base: c.Base, Head: c.Head, ExpectTree: c.Tree})
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
