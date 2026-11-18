package mirror

import (
	"math/rand/v2"
	"time"
)

// Clock is the source of time and of waits. Inside a synctest bubble the real clock is a fake one.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

const (
	backoffBase = time.Second
	backoffCap  = 5 * time.Minute
)

// backoff is the wait after the nth failed attempt (n from 1): 1 s doubled each attempt up to 5 minutes, plus up to 20 percent of jitter.
func backoff(n int) time.Duration {
	d := backoffCap
	if n >= 1 && n <= 20 {
		d = min(backoffBase<<(n-1), backoffCap)
	}
	return d + time.Duration(rand.Float64()*0.2*float64(d))
}
