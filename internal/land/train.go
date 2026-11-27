package land

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"

	"github.com/majorcontext/ward/internal/check"
)

// member is a land of a train, the head that its branch had when the train began, and the result of its trial land.
type member struct {
	l    *Land
	head string
	base string
	gen  int64
	item api.TrialItem
	// from is the commit that the change starts from and fromTree is its tree: the tip for the first member, the trial commit of the member before for a later one.
	from, fromTree string
	// checks are the required checks that judge the member: the ones of the check file of the commit that it replaces.
	checks []check.Check
}

// runTrain judges the lands of ls as one train. It asks motte for a trial land of their changes on the tip, one on the
// result of the one before, runs the checks of each resulting tree, and lands the longest prefix that passed, one motte
// land for each member, in order, each with the tree of its trial. A failure of a read, of the trial or of the check file
// applies to the first land only: the lands after it are judged when they are first.
func (s *Service) runTrain(ctx context.Context, k Key, ls []Land) error {
	reader, merger := s.client(grant.ScopeRead, k.Repo), s.client(grant.ScopeMerge, k.Repo, k.Ref)
	tip, err := reader.Ref(ctx, k.Ref)
	if err != nil {
		return s.conclude(&ls[0], s.refusal(err))
	}
	checks, err := requiredChecks(ctx, reader, k.Repo, tip.Commit)
	var fileErr fileError
	missing := ""
	if err == nil {
		missing = s.missingCheck(k, checks)
	}
	switch {
	case missing != "":
		return s.conclude(&ls[0], outcome{kind: failed, err: &Error{Code: CodeRequired, Message: fmt.Sprintf("the check file of the ref has no check %q", missing), Details: map[string]any{"check": missing}}})
	case errors.As(err, &fileErr):
		return s.conclude(&ls[0], outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: fileErr.Error()}})
	case err != nil:
		return s.conclude(&ls[0], s.refusal(err))
	case len(checks) > 0:
		if _, ok := s.checks.Runner(k.Repo); !ok {
			return s.conclude(&ls[0], s.refusal(check.ErrNoRunner))
		}
	}
	ms, trialErr := s.trial(ctx, reader, merger, k, tip.Commit, ls)
	if trialErr.kind != proceed {
		return s.conclude(&ls[0], trialErr)
	}
	ms = s.policies(ctx, reader, k, checks, ms)
	judged := slices.ContainsFunc(ms, func(m *member) bool { return len(m.checks) > 0 })
	if judged {
		if err := s.starts(ctx, reader, tip.Commit, ms); err != nil {
			return s.conclude(ms[0].l, s.refusal(err))
		}
		if err := s.begin(ctx, ms); err != nil {
			return s.conclude(ms[0].l, s.storeFailure(err))
		}
	}
	pass, out := len(ms), outcome{}
	if judged {
		pass, out = s.judge(ctx, k.Repo, ms)
		if out.kind == aborted {
			return out.storeErr
		}
	}
	if out.kind == failed {
		if err := s.finish(ms[pass].l, out); err != nil {
			return err
		}
	}
	for i := range ms[:pass] {
		ms[i].l.Tree = ms[i].item.Tree
		if err := s.conclude(ms[i].l, s.land(ctx, merger, ms[i].l, ms[i].base, ms[i].head, ms[i].gen)); err != nil {
			return err
		}
		if ms[i].l.State != Landed {
			return s.requeue(ms[i+1 : pass])
		}
	}
	return s.requeue(ms[min(pass+1, len(ms)):])
}

// trial reads the branch of each land and makes the trial land. It returns the members that landed in the trial, up to the
// first change that did not. A first change that did not is the outcome that applies to the first land.
func (s *Service) trial(ctx context.Context, reader, merger *client.Client, k Key, tip string, ls []Land) ([]*member, outcome) {
	ctx, end := span(ctx, "ward.land.trial", attribute.Int("changes", len(ls)))
	var ms []*member
	var changes []api.TrialChange
	for i := range ls {
		branch, err := reader.Ref(ctx, ls[i].Branch)
		if err != nil {
			if i == 0 {
				end(err)
				return nil, s.refusal(err)
			}
			break
		}
		base, err := s.baseOf(ctx, reader, &ls[i], tip, branch.Commit)
		if err != nil {
			if i == 0 {
				end(err)
				return nil, s.refusal(err)
			}
			break
		}
		ms = append(ms, &member{l: &ls[i], head: branch.Commit, base: base, gen: branch.Generation})
		changes = append(changes, api.TrialChange{Head: branch.Commit, Base: base})
	}
	res, err := merger.LandTrial(ctx, k.Repo, api.TrialRequest{Tip: tip, Changes: changes})
	end(err)
	if err != nil {
		return nil, s.refusal(err)
	}
	if len(res.Items) != len(ms) {
		s.log.Error("land", "error", "the trial answered a different number of items")
		return nil, outcome{kind: failed, err: &Error{Code: CodeInternal, Message: "internal error"}}
	}
	for i, item := range res.Items {
		if item.Tip == "" {
			if i == 0 {
				return nil, trialFailure(s, item)
			}
			ms = ms[:i]
			break
		}
		ms[i].item = item
	}
	return ms, outcome{}
}

// trialFailure maps a change that did not land in a trial to the end of its land.
func trialFailure(s *Service, item api.TrialItem) outcome {
	switch {
	case item.Conflict != nil:
		paths := item.Conflict.Paths
		if paths == nil {
			paths = []string{}
		}
		return outcome{kind: failed, err: &Error{Code: CodeConflict, Message: "the branch changes paths that changed on the ref", Details: map[string]any{"paths": paths}}}
	case item.Error != nil && refusedCode(item.Error.Code):
		return outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: "the branch cannot land on the ref"}}
	}
	s.log.Error("land", "error", "a trial land failed")
	return outcome{kind: failed, err: &Error{Code: CodeInternal, Message: "internal error"}}
}

// policies sets the checks of each member. The first member is judged by the check file of the tip, and each later member
// by the check file of the trial commit before it, because a member that changes the file changes the rules of the members
// after it. A member that has no readable file, or needs a runner that the repository lacks, ends the train before it: it is
// judged when it is first, against the real tip.
func (s *Service) policies(ctx context.Context, reader *client.Client, k Key, first []check.Check, ms []*member) []*member {
	ms[0].checks = first
	for i := 1; i < len(ms); i++ {
		checks, err := requiredChecks(ctx, reader, k.Repo, ms[i-1].item.Tip)
		if _, hasRunner := s.checks.Runner(k.Repo); err != nil || len(checks) > 0 && !hasRunner || s.missingCheck(k, checks) != "" {
			return ms[:i]
		}
		ms[i].checks = checks
	}
	return ms
}

// starts sets the commit and the tree that each member starts from.
func (s *Service) starts(ctx context.Context, reader *client.Client, tip string, ms []*member) error {
	c, err := reader.Commit(ctx, tip)
	if err != nil {
		return err
	}
	from, fromTree := tip, c.Tree
	for _, m := range ms {
		m.from, m.fromTree = from, fromTree
		from, fromTree = m.item.Tip, m.item.Tree
	}
	return nil
}

// begin sets each member to checking on the tree of its trial. A member that was checking this tree keeps its mark for the
// fails, so a fail from before a restart still ends it. The others take a new mark.
func (s *Service) begin(ctx context.Context, ms []*member) error {
	var fresh int64
	for _, m := range ms {
		if m.l.State == Checking && m.l.Tree == m.item.Tree && len(m.l.Checks) == len(m.checks) {
			continue
		}
		if fresh == 0 {
			var err error
			if fresh, err = s.checks.Mark(ctx); err != nil {
				return err
			}
		}
		m.l.Mark, m.l.Tree, m.l.Call, m.l.Checks = fresh, m.item.Tree, nil, make([]CheckState, len(m.checks))
		for i, c := range m.checks {
			m.l.Checks[i] = CheckState{Name: c.Name, State: check.Pending}
		}
	}
	for _, m := range ms {
		if err := s.save(m.l, Checking); err != nil {
			return err
		}
	}
	return nil
}

// requeue sets the members that the train did not end back to the queue.
func (s *Service) requeue(ms []*member) error {
	for _, m := range ms {
		if err := s.save(m.l, Queued); err != nil {
			return err
		}
	}
	return nil
}

// conclude applies the end of one pass to a land: it records an end, counts a retry, or keeps a call that had no answer.
func (s *Service) conclude(l *Land, out outcome) error {
	switch out.kind {
	case aborted:
		return out.storeErr
	case landed, failed:
		return s.finish(l, out)
	case retry:
		if out.keepCall {
			s.pause(s.ctx)
			return nil
		}
		if l.Retries >= maxRetries {
			return s.finish(l, outcome{kind: failed, err: &Error{Code: out.retryCode, Message: "the ref kept moving or motte kept timing out"}})
		}
		l.Retries++
		l.Call = nil
		return s.save(l, Queued)
	}
	return nil
}

// refusedCode reports whether a trial item with this code says that the change itself cannot land.
func refusedCode(c api.Code) bool {
	return c == api.CodeLandInvalid || c == api.CodeNotFound || c == api.CodeObjectTooLarge
}
