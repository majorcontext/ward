package mirror

import (
	"testing"
	"time"
)

func TestBackoffDoublesToTheCapWithJitter(t *testing.T) {
	for n, base := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 9: 256 * time.Second, 10: backoffCap, 100: backoffCap} {
		for range 50 {
			if d := backoff(n); d < base || d > base+base/5 {
				t.Fatalf("backoff(%d) = %v, want %v to %v", n, d, base, base+base/5)
			}
		}
	}
}
