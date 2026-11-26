package mirror

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/majorcontext/ward/internal/gitremote"
	"slices"
	"sort"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"
)

const (
	seedStarted = "started"
	seedDone    = "done"

	// StateSeeding and StateSeedRefused are the mirror states that a seed reports.
	StateSeeding     = "seeding"
	StateSeedRefused = "seed_refused"

	// DefaultSeedMaxObjects and DefaultSeedMaxBytes are the size of one seed push. Both are below the limits of a push to motte.
	DefaultSeedMaxObjects = 50000
	DefaultSeedMaxBytes   = 32 << 20
	// DefaultSeedMaxInflated bounds the whole size of the objects of one seed push, as motte bounds its inflated data.
	DefaultSeedMaxInflated = 32 << 20

	emptyTree     = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	fetchBatch    = 200
	seedFetchSpec = "+%s:%s"
)

var errSeedRefused = errors.New("seed refused")

func (e *Engine) setSeedState(name, state, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state == "" {
		delete(e.seedStates, name)
		return
	}
	e.seedStates[name] = seedStatus{state, message}
}

type seedStatus struct{ state, message string }

// SeedState returns StateSeeding or StateSeedRefused while a seed of the mirror runs or was refused, else empty, and the
// reason of a refusal.
func (e *Engine) SeedState(name string) (state, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.seedStates[name]
	return st.state, st.message
}

func refusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errSeedRefused, fmt.Sprintf(format, args...))
}

// checkSeedRefs refuses when a ref of motte holds a value that the seed neither wrote nor found at its start.
func checkSeedRefs(motte []api.Ref, expect map[string]SeedRef) error {
	cur := map[string]string{}
	for _, r := range motte {
		cur[r.Name] = r.Commit
		if x, ok := expect[r.Name]; !ok || (r.Commit != x.Commit && r.Commit != x.Next) {
			return refusal("the motte ref %s changed since the seed wrote it", r.Name)
		}
	}
	for name, x := range expect {
		if x.Commit != "" && cur[name] == "" {
			return refusal("the motte ref %s was deleted since the seed wrote it", name)
		}
	}
	return nil
}

// unseeded reports whether the refs are the ones that a new repository of motte has: main at a root commit with an empty tree.
func unseeded(ctx context.Context, r *client.Client, refs []api.Ref) (bool, error) {
	if len(refs) != 1 || refs[0].Name != "main" {
		return false, nil
	}
	obj, err := r.Object(ctx, refs[0].Commit)
	if err != nil {
		return false, err
	}
	if bytes.Contains(obj.Data, []byte("\nparent ")) {
		return false, nil
	}
	c, err := r.Commit(ctx, refs[0].Commit)
	if err != nil {
		return false, err
	}
	return c.Tree == emptyTree, nil
}

// seed copies the matching refs of the remote into the motte repository, once.
func (e *Engine) seed(ctx context.Context, en Entry) (err error) {
	e.setSeedState(en.Name, StateSeeding, "")
	defer func() {
		switch {
		case err == nil:
			e.setSeedState(en.Name, "", "")
		case errors.Is(err, errSeedRefused):
			e.m.seeds.WithLabelValues(en.Name, "refused").Inc()
			e.setSeedState(en.Name, StateSeedRefused, err.Error())
			e.notify("seed_refused", en.Name)
		}
	}()
	reader := e.motte.reader(en.Repo)
	refs, err := reader.Refs(ctx)
	if err != nil {
		return err
	}
	state, err := e.store.Seed(ctx, en.Name)
	if err != nil {
		return err
	}
	if state != seedStarted {
		ok, err := unseeded(ctx, reader, refs)
		if err != nil {
			return err
		}
		if !ok {
			return refusal("the motte repository already has history")
		}
		if err := e.store.SetSeedRef(ctx, en.Name, "main", SeedRef{Commit: refs[0].Commit}); err != nil {
			return err
		}
		if err := e.store.SetSeed(ctx, en.Name, seedStarted); err != nil {
			return err
		}
	}
	expect, err := e.store.SeedRefs(ctx, en.Name)
	if err != nil {
		return err
	}
	if err := checkSeedRefs(refs, expect); err != nil {
		return err
	}
	sctx, end := e.span(ctx, "ward.mirror.seed")
	err = e.copyRemote(sctx, en, refs)
	end()
	if err != nil {
		e.m.seeds.WithLabelValues(en.Name, "failed").Inc()
		return err
	}
	if err := e.store.SetSeed(ctx, en.Name, seedDone); err != nil {
		return err
	}
	e.m.seeds.WithLabelValues(en.Name, "seeded").Inc()
	e.notify("seed", en.Name)
	return nil
}

// copyRemote runs the copy under the lock of the clone. A git failure that is neither the network nor motte nor the
// remote deletes the clone and runs the copy once more, as a batch does.
func (e *Engine) copyRemote(ctx context.Context, en Entry, motteRefs []api.Ref) error {
	lock := e.repos.get(en.Repo)
	lock.Lock()
	defer lock.Unlock()
	err := e.copyOnce(ctx, en, motteRefs)
	if err != nil && localGitFailure(err) {
		e.wipeClone(en.Repo)
		err = e.copyOnce(ctx, en, motteRefs)
	}
	return err
}

func localGitFailure(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && !transportFailure(err) && classifyRemote(err) == ClassLocal
}

func (e *Engine) copyOnce(ctx context.Context, en Entry, motteRefs []api.Ref) error {
	clone, err := e.ensureClone(ctx, en.Repo)
	if err != nil {
		return err
	}
	tips, err := e.fetchRemoteRefs(ctx, en, clone)
	if err != nil {
		return err
	}
	sort.Slice(tips, func(i, j int) bool {
		if tips[i].Time != tips[j].Time {
			return tips[i].Time < tips[j].Time
		}
		return tips[i].Git < tips[j].Git
	})
	var held, motteCommits []string
	current := map[string]string{}
	for _, r := range motteRefs {
		current[r.Name] = r.Commit
		motteCommits = append(motteCommits, r.Commit)
	}
	reader, err := e.git.openReader(ctx, clone)
	if err != nil {
		return err
	}
	defer reader.close()
	for i, have := range reader.commitsOf(motteCommits) {
		if have != "" {
			held = append(held, motteCommits[i])
		}
	}
	tipNames := make([]string, len(tips))
	for i, t := range tips {
		tipNames[i] = t.Tip
	}
	peeled := reader.commitsOf(tipNames)
	for i, t := range tips {
		stored, _ := StoredRef(t.Git)
		if current[stored] == t.Tip {
			continue
		}
		if err := e.pushSeedRef(ctx, en, clone, reader, t, peeled[i], stored, current[stored], &held); err != nil {
			return err
		}
	}
	return nil
}

// fetchRemoteRefs fetches every ref of the remote that a pattern matches and returns their tips.
func (e *Engine) fetchRemoteRefs(ctx context.Context, en Entry, clone string) ([]seedTip, error) {
	creds, release, err := e.remoteCreds(ctx, en)
	if err != nil {
		return nil, err
	}
	onRemote, err := e.remote.List(ctx, en.Remote, creds)
	release()
	if err != nil {
		return nil, err
	}
	var specs []string
	for git := range onRemote {
		if stored, ok := StoredRef(git); ok && en.Matches(stored) {
			specs = append(specs, fmt.Sprintf(seedFetchSpec, git, localRef(git)))
		}
	}
	sort.Strings(specs)
	for batch := range slices.Chunk(specs, fetchBatch) {
		creds, release, err := e.remoteCreds(ctx, en)
		if err != nil {
			return nil, err
		}
		err = e.git.fetch(ctx, clone, en.Remote, creds, batch)
		release()
		if err != nil {
			return nil, err
		}
	}
	all, err := e.git.seedTips(ctx, clone)
	if err != nil {
		return nil, err
	}
	var tips []seedTip
	for _, t := range all {
		if _, ok := onRemote[t.Git]; ok {
			if stored, ok := StoredRef(t.Git); ok && en.Matches(stored) {
				tips = append(tips, t)
			}
		}
	}
	return tips, nil
}

// pushSeedRef moves one ref of motte from lease to the tip in chunks. held lists the commits that motte holds.
func (e *Engine) pushSeedRef(ctx context.Context, en Entry, clone string, sizer *objReader, t seedTip, commit, stored, lease string, held *[]string) error {
	var chain []string
	if commit != "" {
		var err error
		if chain, err = e.git.firstParents(ctx, clone, commit, *held); err != nil {
			return err
		}
	}
	var probeErr error
	fits := func(from, to int) bool {
		not := slices.Clone(*held)
		if from >= 0 {
			not = append(not, chain[from])
		}
		ok, err := e.git.fitsPush(ctx, sizer, clone, chain[to], not, e.cfg.SeedMaxObjects, e.cfg.SeedMaxBytes, e.cfg.SeedMaxInflated)
		if err != nil {
			probeErr = err
			return true
		}
		return ok
	}
	var steps []string
	for _, i := range planChunks(len(chain), fits) {
		steps = append(steps, chain[i])
	}
	if probeErr != nil {
		return probeErr
	}
	if len(steps) == 0 || steps[len(steps)-1] != t.Tip {
		steps = append(steps, t.Tip)
	}
	for _, step := range steps {
		creds, err := e.motte.mergeCreds(en.Repo, stored)
		if err != nil {
			return err
		}
		if err := e.store.SetSeedRef(ctx, en.Name, stored, SeedRef{Commit: lease, Next: step}); err != nil {
			return err
		}
		pctx, end := e.span(ctx, "ward.mirror.seed.push")
		res, err := e.remote.Push(pctx, clone, e.motte.gitURL(en.Repo), creds, []gitremote.PushSpec{{Ref: t.Git, Commit: step, Lease: lease, LeaseSet: true}})
		end()
		if err != nil {
			return err
		}
		if res[t.Git] != gitremote.OK || !hasResult(res, t.Git) {
			return fmt.Errorf("motte refused the push of %s", t.Git)
		}
		if err := e.store.SetSeedRef(ctx, en.Name, stored, SeedRef{Commit: step}); err != nil {
			return err
		}
		lease = step
		*held = append(*held, step)
	}
	return nil
}

// cleanSeed deletes from motte the refs that an earlier seed wrote and that are not mirrorable, once. It deletes a ref
// only while motte holds a value that the seed record says ward wrote, and it never touches any other ref.
func (e *Engine) cleanSeed(ctx context.Context, en Entry) error {
	if done, err := e.store.SeedCleaned(ctx, en.Name); done || err != nil {
		return err
	}
	if state, err := e.store.Seed(ctx, en.Name); state == "" || err != nil {
		return err
	}
	expect, err := e.store.SeedRefs(ctx, en.Name)
	if err != nil {
		return err
	}
	var stale []string
	for name := range expect {
		if !Mirrorable(name) {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		if err := e.deleteSeeded(ctx, en, stale, expect); err != nil {
			return err
		}
	}
	return e.store.FinishSeedCleanup(ctx, en.Name, stale)
}

func (e *Engine) deleteSeeded(ctx context.Context, en Entry, stale []string, expect map[string]SeedRef) error {
	refs, err := e.motte.reader(en.Repo).Refs(ctx)
	if err != nil {
		return err
	}
	current := map[string]string{}
	for _, r := range refs {
		current[r.Name] = r.Commit
	}
	var specs []gitremote.PushSpec
	var named []string
	for _, name := range stale {
		cur, x := current[name], expect[name]
		git, ok := GitRef(name)
		switch {
		case cur == "" || !ok:
		case cur == x.Commit || cur == x.Next:
			specs = append(specs, gitremote.PushSpec{Ref: git, Lease: cur, LeaseSet: true})
			named = append(named, name)
		default:
			e.log.Warn("seed cleanup skipped a ref that motte holds at another value", "mirror", en.Name, "ref", name)
		}
	}
	if len(specs) == 0 {
		return nil
	}
	lock := e.repos.get(en.Repo)
	lock.Lock()
	defer lock.Unlock()
	clone, err := e.ensureClone(ctx, en.Repo)
	if err != nil {
		return err
	}
	for from := 0; from < len(specs); from += grant.MaxRefs {
		to := min(from+grant.MaxRefs, len(specs))
		creds, err := e.motte.mergeCreds(en.Repo, named[from:to]...)
		if err != nil {
			return err
		}
		res, err := e.remote.Push(ctx, clone, e.motte.gitURL(en.Repo), creds, specs[from:to])
		if err != nil {
			return err
		}
		for _, sp := range specs[from:to] {
			if !hasResult(res, sp.Ref) || res[sp.Ref] != gitremote.OK {
				return fmt.Errorf("motte refused the delete of %s", sp.Ref)
			}
		}
	}
	return nil
}
