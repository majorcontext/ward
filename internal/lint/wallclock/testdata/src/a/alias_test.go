package a

import (
	"testing"
	st "testing/synctest"
	clk "time"
)

func TestAliasBubble(t *testing.T) {
	st.Test(t, func(t *testing.T) { clk.Sleep(1) })
}

func TestAliasOutside(t *testing.T) {
	clk.Sleep(1)   // want `time.Sleep outside a synctest bubble`
	<-clk.After(1) // want `time.After outside a synctest bubble`
}
