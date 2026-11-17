package a

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(1)
		<-time.After(1)
		sleeper()
	})
}

func TestOutside(t *testing.T) {
	time.Sleep(1)   // want `time.Sleep outside a synctest bubble`
	<-time.After(1) // want `time.After outside a synctest bubble`
	sleeper()       // want `call to sleeper, which waits on the wall clock, outside a synctest bubble`
	chained()       // want `call to chained, which waits on the wall clock, outside a synctest bubble`
	synctest.Test(t, func(t *testing.T) {})
	time.Sleep(1) // want `time.Sleep outside a synctest bubble`
}

func TestAllowed(t *testing.T) {
	time.Sleep(1) //lint:allow wallclock cross-process wait
}

func sleeper() { // want sleeper:"waits"
	time.Sleep(1) // want `time.Sleep outside a synctest bubble`
}

func chained() { sleeper() } // want chained:"waits" `call to sleeper, which waits on the wall clock, outside a synctest bubble`

func onlyInBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) { time.Sleep(1) })
}

type fake struct{}

func (fake) Sleep(time.Duration) {}

func TestOtherSleep(t *testing.T) {
	var f fake
	f.Sleep(1)
	onlyInBubble(nil)
}
