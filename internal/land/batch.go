package land

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"go.opentelemetry.io/otel/attribute"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/check"
)

// member is a land of a batch and the candidate that its change makes on top of the changes before it.
type member struct {
	l       *Land
	gen     int64
	commit  string
	tree    string
	ff      bool
	commits []api.LandedCommit
}

func scratchRef(id string) string { return candidateRef("batch-" + id) }

func (s *Service) dropRef(reader, merger *client.Client, name string) {
	if ref, err := reader.Ref(s.ctx, name); err == nil {
		_ = merger.DeleteRef(s.ctx, name, ref.Generation)
	}
}

func lands(ms []*member) []*Land {
	out := make([]*Land, len(ms))
	for i, m := range ms {
		out[i] = m.l
	}
	return out
}

// runBatch judges the lands of ls with one check run on the tree that all their changes make, and lands them together.
// A fail makes it find the first change that fails by a binary search over the prefixes of the batch. That change fails,
// the prefix before it lands, and the changes after it stay queued.
func (s *Service) runBatch(ctx context.Context, k Key, ls []Land) error {
	reader, merger := s.client("read", k.Repo), s.client("merge", k.Repo)
	name := scratchRef(ls[0].ID)
	defer s.dropRef(reader, merger, name)
	tip, err := reader.Ref(ctx, k.Ref)
	if err != nil {
		return s.settle(ptrs(ls), s.refusal(err))
	}
	bctx, endBuild := span(ctx, "ward.land.build")
	ms, rest, out := s.build(bctx, reader, merger, ls, tip.Commit, name)
	endBuild(nil, attribute.Int("members", len(ms)))
	if out.kind != proceed {
		return s.settle(rest, out)
	}
	if len(ms) == 0 {
		return nil
	}
	checks, err := requiredChecks(ctx, reader, tip.Commit)
	var fileErr fileError
	switch {
	case errors.As(err, &fileErr):
		return s.settle(lands(ms), outcome{kind: failed, err: &Error{Code: CodeInvalid, Message: fileErr.Error()}})
	case err != nil:
		return s.settle(lands(ms), s.refusal(err))
	case len(checks) > 0:
		if _, ok := s.checks.Runner(k.Repo); !ok {
			return s.settle(lands(ms), s.refusal(check.ErrNoRunner))
		}
	}
	mark, err := s.begin(ctx, ms, checks)
	if err != nil {
		return s.settle(nil, s.storeFailure(err))
	}
	judge := func(n int) (outcome, []check.Verdict) {
		var last []check.Verdict
		ctx, end := span(ctx, "ward.land.judge", attribute.Int("prefix", n))
		defer func() { end(nil) }()
		out := s.await(ctx, k.Repo, ms[n-1].tree, ms[n-1].commit, mark, checks, func(vs []check.Verdict) error {
			last = vs
			for _, m := range ms {
				setStates(m.l, vs)
				if err := s.save(m.l, m.l.State); err != nil {
					return err
				}
			}
			return nil
		})
		return out, last
	}
	n := len(ms)
	out, verdicts := judge(n)
	if out.kind == failed {
		lo, hi, failVerdicts := 0, n, verdicts
		for hi-lo > 1 {
			mid := (lo + hi) / 2
			o, vs := judge(mid)
			switch o.kind {
			case proceed:
				lo, verdicts = mid, vs
			case failed:
				hi, out, failVerdicts = mid, o, vs
			default:
				return s.settle(lands(ms), o)
			}
		}
		bad := ms[hi-1]
		setStates(bad.l, failVerdicts)
		if err := s.finish(bad.l, out); err != nil {
			return err
		}
		for _, m := range ms[hi:] {
			if err := s.save(m.l, Queued); err != nil {
				return err
			}
		}
		s.adapt(k, false)
		if ms = ms[:lo]; len(ms) == 0 {
			return nil
		}
	} else if out.kind != proceed {
		return s.settle(lands(ms), out)
	}
	for _, m := range ms {
		setStates(m.l, verdicts)
	}
	cctx, endCommit := span(ctx, "ward.land.commit", attribute.Int("members", len(ms)))
	err = s.commit(cctx, merger, k, ms, tip.Commit)
	endCommit(err)
	if err != nil {
		return err
	}
	if n == len(ms) {
		s.adapt(k, true)
	}
	return nil
}

func ptrs(ls []Land) []*Land {
	out := make([]*Land, len(ls))
	for i := range ls {
		out[i] = &ls[i]
	}
	return out
}

func setStates(l *Land, vs []check.Verdict) {
	for i, v := range vs {
		l.Checks[i].State = v.State
	}
}

// build lands the change of each land of ls, in order, on a scratch ref that starts at tip. A land that fails alone ends.
// A change that conflicts with an earlier change of the batch and not with the tip ends the batch before it. When the
// outcome is not proceed, rest holds the lands that the outcome applies to.
func (s *Service) build(ctx context.Context, reader, merger *client.Client, ls []Land, tip, name string) (ms []*member, rest []*Land, out outcome) {
	if old, err := reader.Ref(ctx, name); err == nil {
		if err := merger.DeleteRef(ctx, name, old.Generation); err != nil {
			return nil, ptrs(ls), s.refusal(err)
		}
	}
	if _, err := merger.SwapRef(ctx, name, 0, tip); err != nil {
		return nil, ptrs(ls), s.refusal(err)
	}
	for i := range ls {
		m, out := s.add(ctx, reader, merger, &ls[i], tip, name, len(ms) > 0)
		switch {
		case out.kind == failed:
			if err := s.finish(&ls[i], out); err != nil {
				return nil, nil, outcome{kind: aborted, storeErr: err}
			}
		case out.kind != proceed:
			return nil, append(lands(ms), &ls[i]), out
		case m == nil:
			return ms, nil, outcome{}
		default:
			ms = append(ms, m)
		}
	}
	return ms, nil, outcome{}
}

// add lands the change of l on the scratch ref. A nil member with a proceed outcome means that the change must wait.
func (s *Service) add(ctx context.Context, reader, merger *client.Client, l *Land, tip, name string, later bool) (*member, outcome) {
	branch, err := reader.Ref(ctx, l.Branch)
	if err != nil {
		return nil, s.refusal(err)
	}
	res, err := merger.Land(ctx, l.Repo, api.LandRequest{Ref: name, Base: l.Base, Head: branch.Commit})
	if later && errors.Is(err, client.ErrLandConflict) {
		return nil, outcome{}
	}
	if err != nil {
		return nil, s.refusal(err)
	}
	c, err := reader.Commit(ctx, res.Tip)
	if err != nil {
		if out := s.refusal(err); out.kind != failed {
			return nil, out
		}
		return nil, outcome{kind: retry, retryCode: CodeInternal}
	}
	return &member{l: l, gen: branch.Generation, commit: res.Tip, tree: c.Tree, ff: res.FastForward, commits: res.Commits}, outcome{}
}

// begin sets each member to checking on its own tree. It returns the mark for the fails of this run.
func (s *Service) begin(ctx context.Context, ms []*member, checks []check.Check) (int64, error) {
	resumed := true
	for _, m := range ms {
		resumed = resumed && m.l.State == Checking && m.l.Tree == m.tree
	}
	mark := ms[0].l.Mark
	if !resumed {
		var err error
		if mark, err = s.checks.Mark(ctx); err != nil {
			return 0, err
		}
	}
	for _, m := range ms {
		m.l.Mark, m.l.Tree, m.l.Call, m.l.Checks = mark, m.tree, nil, make([]CheckState, len(checks))
		for i, c := range checks {
			m.l.Checks[i] = CheckState{Name: c.Name, State: check.Pending}
		}
		if err := s.save(m.l, Checking); err != nil {
			return 0, err
		}
	}
	return mark, nil
}

// commit lands the checked prefix of the batch as one fast-forward of the tip, so the ref only holds a tree that passed.
// It stores the call of each land first, the first land last, so that a restart finds the whole batch or none of it.
func (s *Service) commit(ctx context.Context, merger *client.Client, k Key, ms []*member, tip string) error {
	for _, m := range ms {
		m.l.Attempt++
	}
	last := ms[len(ms)-1]
	id := fmt.Sprintf("%s-b%d", ms[0].l.ID, ms[0].l.Attempt)
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		m.l.Call = &Call{Base: tip, Head: last.commit, Tree: last.tree, BranchGen: m.gen, ID: id, Tip: m.commit, FastForward: m.ff, Commits: m.commits}
		if err := s.save(m.l, Landing); err != nil {
			return err
		}
	}
	return s.sendBatch(ctx, merger, k, lands(ms))
}

// resumeBatch repeats the stored call of the lands that a restart found in the state landing.
func (s *Service) resumeBatch(ctx context.Context, k Key, ls []Land) error {
	var group []*Land
	for i := range ls {
		if ls[i].State == Landing && ls[i].Call != nil && ls[i].Call.ID == ls[0].Call.ID {
			group = append(group, &ls[i])
		}
	}
	reader, merger := s.client("read", k.Repo), s.client("merge", k.Repo)
	defer s.dropRef(reader, merger, scratchRef(group[0].ID))
	return s.sendBatch(ctx, merger, k, group)
}

// sendBatch is the land call of motte for a batch. Motte answers a repeat of the same id with the first result.
func (s *Service) sendBatch(ctx context.Context, merger *client.Client, k Key, group []*Land) error {
	c := group[0].Call
	ctx, end := span(ctx, "ward.land.send", attribute.Int("members", len(group)))
	res, err := merger.Land(ctx, k.Repo, api.LandRequest{ID: c.ID, Ref: k.Ref, Base: c.Base, Head: c.Head, ExpectTree: c.Tree})
	end(err)
	switch {
	case err == nil:
		moved := map[string]string{}
		for _, c := range res.Commits {
			moved[c.Old] = c.New
		}
		for _, l := range group {
			own := l.Call
			resp := api.LandResponse{Tip: own.Tip, Generation: res.Generation, FastForward: own.FastForward, Commits: own.Commits}
			if !res.FastForward {
				resp.Tip, resp.Commits = cmp.Or(moved[own.Tip], own.Tip), slices.Clone(own.Commits)
				for i, c := range resp.Commits {
					resp.Commits[i].New = cmp.Or(moved[c.New], c.New)
				}
			}
			if err := s.finish(l, outcome{kind: landed, landed: resp, branchGen: own.BranchGen}); err != nil {
				return err
			}
		}
		return nil
	case s.ctx.Err() == nil && noAnswer(err):
		if err := s.settle(group, outcome{kind: retry, retryCode: CodeTimeout, keepCall: true}); err != nil {
			return err
		}
		if !s.pause(ctx) {
			return ctx.Err()
		}
		return nil
	case errors.Is(err, client.ErrLandConflict):
		return s.settle(group, outcome{kind: retry, retryCode: CodeStale})
	case errors.Is(err, client.ErrLandInvalid) && len(group) > 1:
		s.mu.Lock()
		s.sizes[k] = 1
		s.mu.Unlock()
		return s.settle(group, outcome{kind: retry, retryCode: CodeInvalid})
	}
	return s.settle(group, s.refusal(err))
}

// settle applies an outcome that holds for every land of ls.
func (s *Service) settle(ls []*Land, out outcome) error {
	switch out.kind {
	case aborted:
		return out.storeErr
	case failed:
		for _, l := range ls {
			if err := s.finish(l, out); err != nil {
				return err
			}
		}
	case retry:
		for _, l := range ls {
			if err := s.retryLand(l, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) retryLand(l *Land, out outcome) error {
	if out.keepCall {
		return nil
	}
	if l.Retries >= maxRetries {
		return s.finish(l, outcome{kind: failed, err: &Error{Code: out.retryCode, Message: "the ref kept moving or motte kept timing out"}})
	}
	l.Retries++
	l.Call = nil
	return s.save(l, Queued)
}
