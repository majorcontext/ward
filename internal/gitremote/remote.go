// Package gitremote is the seam between the mirror and the git remotes that it talks to: listing the refs of a remote and pushing
// refs to it. The mirror owns the interface. gitbin runs the git binary, and fakeremote keeps the refs in memory.
package gitremote

import "context"

// Names of the environment variables that tell the askpass helper which file and user to answer with.
const (
	AskpassFileEnv = "WARD_ASKPASS_FILE"
	AskpassUserEnv = "WARD_ASKPASS_USER"
)

// Cred is the credential of one run. Header is a complete HTTP header line. AskFile and AskUser feed the askpass helper.
type Cred struct {
	Header  string
	AskUser string
	AskFile string
	// KeepGC leaves gc.auto at its default, for the explicit maintenance command of a git run.
	KeepGC bool
}

// PushSpec is one ref of a push. An empty Commit deletes the ref. With LeaseSet, the push moves the ref only when the remote
// holds Lease, and an empty Lease means that the ref must be absent.
type PushSpec struct {
	Ref      string
	Commit   string
	Lease    string
	LeaseSet bool
}

// Outcome is the result of one ref of a push.
type Outcome int

const (
	// OK means that the remote holds the pushed value, or lacks a ref that the push deleted.
	OK Outcome = iota
	// Stale means that the lease refused the ref: the remote moved outside the caller.
	Stale
	// Rejected means that the remote refused the ref for another reason.
	Rejected
)

// Remote is a git remote.
type Remote interface {
	// List returns the refs of the remote under refs/ that name a commit, as git name to commit. It skips HEAD and peeled tags.
	List(ctx context.Context, url string, c Cred) (map[string]string, error)
	// Push sends the specs in one push from the repository at clone, and reports the outcome of each ref. A delete of a ref that
	// the remote lacks is OK. An error means that the push as a whole failed.
	Push(ctx context.Context, clone, url string, c Cred, specs []PushSpec) (map[string]Outcome, error)
}

// Error is a failure of a git run. Stderr is its error output, and TimedOut is true when the time limit ended it.
type Error struct {
	Stderr   string
	Err      error
	TimedOut bool
}

func (e *Error) Error() string { return "git: " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }
