package mirror

import (
	"context"
	"time"

	"github.com/majorcontext/motte/api"
)

// Class is the class of a failed action.
type Class string

const (
	ClassMotte             Class = "motte"
	ClassRemoteUnreachable Class = "remote_unreachable"
	ClassRemoteAuth        Class = "remote_auth"
	ClassRemoteRejected    Class = "remote_rejected"
	ClassLocal             Class = "local"
)

// Ref is the state of one ref of one mirror. PushedSet is false while ward knows nothing about the remote value.
type Ref struct {
	Mirror        string
	Name          string
	LastPartition int
	LastSeq       int64
	Dirty         bool
	Version       int64
	DirtySince    time.Time
	Pushed        string
	PushedSet     bool
	Attempts      int
	NextAttempt   time.Time
	Failure       Class
	Message       string
	FailureSince  time.Time
	PushedAt      time.Time
}

// SeedRef is the motte value that a seed expects for a ref. Commit is the last value that ward wrote or found (empty for
// absence). Next is the value of a push that may or may not have landed (empty when none is under way).
type SeedRef struct{ Commit, Next string }

// Service is the state that belongs to the process and not to a mirror.
type Service struct {
	SubscriptionID string
	EventsCursor   string
	LastSeq        map[int]int64
}

// Hit is an event and the names of the mirrors whose patterns match it.
type Hit struct {
	Event   api.Event
	Mirrors []string
}

// HookResult is how the sequence of a delivered event compares with the partition state.
type HookResult int

const (
	HookBaseline HookResult = iota
	HookInOrder
	HookRepeat
	HookGap
)

// Stats summarises the refs of one mirror.
type Stats struct {
	Mirror      string
	Reconciled  bool
	Dirty       int
	Failing     map[Class]int
	OldestDirty time.Time
}

// Store is the transactional state store. Each method that writes is one transaction.
type Store interface {
	Ping(ctx context.Context) error
	Service(ctx context.Context) (Service, error)
	// SetSubscription stores the subscription and the events cursor. With clearSeq it also clears last_seq of every partition.
	SetSubscription(ctx context.Context, id, cursor string, clearSeq bool) error
	// RecordHook compares the sequence of the event with its partition, stores last_seq and the triggers, and says how.
	RecordHook(ctx context.Context, hit Hit, now time.Time) (HookResult, error)
	// RecordPage stores the triggers of a catch-up page, last_seq where it is higher, and the new cursor.
	RecordPage(ctx context.Context, hits []Hit, cursor string, now time.Time) error
	// Due returns the dirty refs of a mirror that may be acted on now, and the earliest later next_attempt (zero if none).
	Due(ctx context.Context, mirror string, now time.Time) ([]Ref, time.Time, error)
	// RecordPush stores a push result. It clears dirty only when the version of the ref is still version.
	RecordPush(ctx context.Context, mirror, ref string, version int64, pushed string, now time.Time) error
	// RecordFailure counts a failed action. It moves next_attempt only when the version of the ref is still version.
	RecordFailure(ctx context.Context, mirror, ref string, version int64, class Class, message string, next, now time.Time) error
	SetPushed(ctx context.Context, mirror, ref, pushed string) error
	// Reconciled triggers each ref of targets, sets its pushed value, and records the mirror.
	Reconciled(ctx context.Context, mirror string, targets map[string]string, now time.Time) error
	// Seed returns the seed record of a mirror: empty, started or done.
	Seed(ctx context.Context, mirror string) (string, error)
	SetSeed(ctx context.Context, mirror, state string) error
	// SeedRefs returns what a seed expects motte to hold for each ref it has written or may write.
	SeedRefs(ctx context.Context, mirror string) (map[string]SeedRef, error)
	SetSeedRef(ctx context.Context, mirror, ref string, r SeedRef) error
	// SeedCleaned reports whether the cleanup of the seed record has run.
	SeedCleaned(ctx context.Context, mirror string) (bool, error)
	// FinishSeedCleanup removes refs from the seed record and from the ref state of the mirror, and records the cleanup.
	FinishSeedCleanup(ctx context.Context, mirror string, refs []string) error
	HasMirror(ctx context.Context, name string) (bool, error)
	StateRefs(ctx context.Context, mirror string) ([]string, error)
	Refs(ctx context.Context, mirror, after string, limit int) ([]Ref, error)
	Stats(ctx context.Context) ([]Stats, error)
}
