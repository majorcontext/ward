package mirror

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel"

	"github.com/majorcontext/motte/client"
)

// target is what a worker will make true on the remote for one dirty ref. An empty commit means delete.
type target struct {
	ref    Ref
	git    string
	commit string
	failed bool
}

func (e *Engine) span(ctx context.Context, name string) (context.Context, func()) {
	ctx, s := otel.Tracer("ward").Start(ctx, name)
	return ctx, func() { s.End() }
}

func remoteCreds(en Entry) gitCreds {
	return gitCreds{AskUser: en.Username, AskFile: en.CredentialFile}
}

// batch acts on the due refs of one mirror with one clone. A failure that is neither network nor remote deletes the clone and acts once more.
func (e *Engine) batch(ctx context.Context, en Entry, due []Ref) {
	lock := e.repos.get(en.Repo)
	lock.Lock()
	for try := 0; try < 2; try++ {
		if !e.attempt(ctx, en, due, try == 1) {
			break
		}
		e.wipeClone(en.Repo)
	}
	lock.Unlock()
	e.afterBatch(ctx)
	e.notify("batch", en.Name)
}

func (e *Engine) fail(ctx context.Context, en Entry, r Ref, class Class) {
	if ctx.Err() != nil {
		return
	}
	now := e.clock.Now()
	err := e.store.RecordFailure(ctx, en.Name, r.Name, r.Version, class, failureText[class], now.Add(backoff(r.Attempts+1)), now)
	if err != nil {
		e.log.Error("record failure", "mirror", en.Name, "ref", r.Name, "error", sanitize(err))
	}
	e.m.pushes.WithLabelValues(en.Name, string(class)).Inc()
	e.log.Warn("ref action failed", "mirror", en.Name, "ref", r.Name, "class", class, "attempts", r.Attempts+1)
}

func (e *Engine) succeed(ctx context.Context, en Entry, t *target) {
	if ctx.Err() != nil {
		return
	}
	if err := e.store.RecordPush(ctx, en.Name, t.ref.Name, t.ref.Version, t.commit, e.clock.Now()); err != nil {
		e.log.Error("record push", "mirror", en.Name, "ref", t.ref.Name, "error", sanitize(err))
		return
	}
	result := "pushed"
	if t.commit == "" {
		result = "deleted"
	}
	e.m.pushes.WithLabelValues(en.Name, result).Inc()
}

// readTargets reads each due ref from motte. A ref that motte lacks is a deletion.
func (e *Engine) readTargets(ctx context.Context, en Entry, due []Ref) []*target {
	reader := e.motte.reader(en.Repo)
	var out []*target
	for _, r := range due {
		g, ok := GitRef(r.Name)
		if !ok {
			_ = e.store.RecordPush(ctx, en.Name, r.Name, r.Version, r.Pushed, e.clock.Now())
			continue
		}
		t := &target{ref: r, git: g}
		ref, err := reader.Ref(ctx, r.Name)
		switch {
		case errors.Is(err, client.ErrNotFound):
		case err != nil:
			e.fail(ctx, en, r, ClassMotte)
			continue
		default:
			t.commit = ref.Commit
		}
		out = append(out, t)
	}
	return out
}

// fetchTargets makes the clone hold every commit that the targets name and points the local refs at them.
// It returns true when the clone must be deleted and the batch repeated.
func (e *Engine) fetchTargets(ctx context.Context, en Entry, clone string, targets []*target, final bool) (retry bool) {
	var need []string
	local := map[string]string{}
	for _, t := range targets {
		if t.commit == "" {
			continue
		}
		local[t.git] = t.commit
		if !e.git.hasCommit(ctx, clone, t.commit) {
			need = append(need, t.commit)
		}
	}
	if len(need) > 0 {
		creds, err := e.motte.gitCreds(en.Repo)
		if err == nil {
			fctx, end := e.span(ctx, "ward.mirror.fetch")
			err = e.git.fetch(fctx, clone, e.motte.gitURL(en.Repo), creds, need)
			end()
		}
		if err != nil {
			return e.failAll(ctx, en, targets, func(t *target) bool { return t.commit != "" }, ClassMotte, transportFailure(err), final)
		}
	}
	if len(local) > 0 {
		if err := e.git.setLocalRefs(ctx, clone, local); err != nil {
			return e.failAll(ctx, en, targets, func(*target) bool { return true }, ClassLocal, false, final)
		}
	}
	return false
}

// failAll fails the targets that pick selects with class. A failure that is not a transport failure and is not final asks for a retry.
func (e *Engine) failAll(ctx context.Context, en Entry, targets []*target, pick func(*target) bool, class Class, transport, final bool) bool {
	if !transport && class == ClassMotte {
		class = ClassLocal
	}
	if class == ClassLocal && !final {
		return true
	}
	for _, t := range targets {
		if !t.failed && pick(t) {
			t.failed = true
			e.fail(ctx, en, t.ref, class)
		}
	}
	return false
}

func specsOf(targets []*target) []pushSpec {
	var specs []pushSpec
	for _, t := range targets {
		if !t.failed {
			specs = append(specs, pushSpec{Ref: t.git, Commit: t.commit, Lease: t.ref.Pushed, LeaseSet: t.ref.PushedSet})
		}
	}
	return specs
}

// attempt does one pass over the due refs. It returns true when the clone must be deleted and the pass repeated.
func (e *Engine) attempt(ctx context.Context, en Entry, due []Ref, final bool) (retry bool) {
	targets := e.readTargets(ctx, en, due)
	if len(targets) == 0 {
		return false
	}
	clone, err := e.ensureClone(ctx, en.Repo)
	if err != nil {
		return e.failAll(ctx, en, targets, func(*target) bool { return true }, ClassLocal, false, final)
	}
	if e.fetchTargets(ctx, en, clone, targets, final) {
		return true
	}
	creds := remoteCreds(en)
	pctx, end := e.span(ctx, "ward.mirror.push")
	results, err := e.git.push(pctx, clone, en.Remote, creds, specsOf(targets))
	end()
	if err != nil {
		return e.failAll(ctx, en, targets, func(*target) bool { return true }, classifyRemote(err), true, final)
	}
	stale := e.settle(ctx, en, targets, results)
	if len(stale) > 0 {
		e.reconcileStale(ctx, en, clone, stale)
	}
	e.cleanup(ctx, en, clone, targets)
	return false
}

// settle records the result of each target and returns the targets that the lease refused.
func (e *Engine) settle(ctx context.Context, en Entry, targets []*target, results map[string]outcome) []*target {
	var stale []*target
	for _, t := range targets {
		if t.failed {
			continue
		}
		res, ok := results[t.git]
		switch {
		case ok && res == outcomeOK:
			t.failed = false
			e.succeed(ctx, en, t)
		case ok && res == outcomeStale:
			e.m.pushes.WithLabelValues(en.Name, "stale").Inc()
			stale = append(stale, t)
		case ok:
			t.failed = true
			e.fail(ctx, en, t.ref, ClassRemoteRejected)
		default:
			t.failed = true
			e.fail(ctx, en, t.ref, ClassLocal)
		}
	}
	return stale
}

// reconcileStale reads the remote value of each refused ref, records it as pushed, and pushes again at once.
func (e *Engine) reconcileStale(ctx context.Context, en Entry, clone string, stale []*target) {
	creds := remoteCreds(en)
	tips, err := e.git.lsRemote(ctx, en.Remote, creds)
	if err != nil {
		for _, t := range stale {
			t.failed = true
			e.fail(ctx, en, t.ref, classifyRemote(err))
		}
		return
	}
	for _, t := range stale {
		t.ref.Pushed, t.ref.PushedSet = tips[t.git], true
		if err := e.store.SetPushed(ctx, en.Name, t.ref.Name, t.ref.Pushed); err != nil {
			e.log.Error("record remote value", "mirror", en.Name, "ref", t.ref.Name, "error", sanitize(err))
		}
	}
	results, err := e.git.push(ctx, clone, en.Remote, creds, specsOf(stale))
	for _, t := range stale {
		switch {
		case err != nil:
			t.failed = true
			e.fail(ctx, en, t.ref, classifyRemote(err))
		case results[t.git] == outcomeOK && hasResult(results, t.git):
			e.succeed(ctx, en, t)
		default:
			t.failed = true
			e.fail(ctx, en, t.ref, ClassRemoteRejected)
		}
	}
}

func hasResult(results map[string]outcome, ref string) bool {
	_, ok := results[ref]
	return ok
}

// cleanup drops the local refs of deleted refs and collects garbage: a prune after a delete, else gc --auto.
func (e *Engine) cleanup(ctx context.Context, en Entry, clone string, targets []*target) {
	gone := map[string]string{}
	for _, t := range targets {
		if t.commit == "" && !t.failed {
			gone[t.git] = ""
		}
	}
	if len(gone) > 0 {
		if err := e.git.setLocalRefs(ctx, clone, gone); err != nil {
			e.log.Warn("drop local refs", "mirror", en.Name, "error", sanitize(err))
		}
	}
	if err := e.git.gc(ctx, clone, len(gone) > 0); err != nil && ctx.Err() == nil {
		e.log.Warn("git gc failed", "mirror", en.Name, "error", sanitize(err))
	}
}

// dropExcluded clears the work of refs that no current pattern matches and returns the rest. An excluded ref is never touched on the remote.
func (e *Engine) dropExcluded(ctx context.Context, en Entry, due []Ref) []Ref {
	var keep []Ref
	for _, r := range due {
		if en.Matches(r.Name) {
			keep = append(keep, r)
			continue
		}
		if err := e.store.RecordPush(ctx, en.Name, r.Name, r.Version, r.Pushed, e.clock.Now()); err != nil {
			e.log.Error("clear excluded ref", "mirror", en.Name, "error", sanitize(err))
		}
	}
	return keep
}
