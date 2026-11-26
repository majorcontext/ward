package mirror

import "context"

// reconcile triggers every ref of the mirror that may differ from the remote, and sets what ward believes the remote holds.
func (e *Engine) reconcile(ctx context.Context, en Entry) error {
	refs, err := e.motte.reader(en.Repo).Refs(ctx)
	if err != nil {
		return err
	}
	creds, release, err := e.remoteCreds(ctx, en)
	if err != nil {
		return err
	}
	onGit, err := e.remote.List(ctx, en.Remote, creds)
	release()
	if err != nil {
		return err
	}
	onRemote := map[string]string{}
	for git, sha := range onGit {
		if stored, ok := StoredRef(git); ok && en.Matches(stored) {
			onRemote[stored] = sha
		}
	}
	inMotte := map[string]bool{}
	targets := map[string]string{}
	for _, r := range refs {
		if en.Matches(r.Name) {
			inMotte[r.Name] = true
			targets[r.Name] = onRemote[r.Name]
		}
	}
	known, err := e.store.StateRefs(ctx, en.Name)
	if err != nil {
		return err
	}
	for _, name := range known {
		if !inMotte[name] && en.Matches(name) {
			targets[name] = onRemote[name]
		}
	}
	if en.PruneUnknown {
		for name, tip := range onRemote {
			if !inMotte[name] {
				targets[name] = tip
			}
		}
	}
	if err := e.store.Reconciled(ctx, en.Name, targets, e.clock.Now()); err != nil {
		return err
	}
	e.wakeMirrors([]string{en.Name})
	e.notify("reconcile", en.Name)
	return nil
}
