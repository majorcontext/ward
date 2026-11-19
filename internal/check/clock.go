package check

import "time"

// Clock is the source of time and of waits.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the clock of the wall.
func RealClock() Clock { return realClock{} }

const (
	backoffBase = time.Second
	backoffCap  = 5 * time.Minute
)

// backoff is the wait after the nth failed attempt (n from 1): 1 s doubled each attempt, up to 5 minutes.
func backoff(n int) time.Duration {
	if n < 1 || n > 20 {
		return backoffCap
	}
	return min(backoffBase<<(n-1), backoffCap)
}
