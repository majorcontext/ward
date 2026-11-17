package a

import (
	"testing"
	. "time"
)

func TestDotOutside(t *testing.T) {
	Sleep(1) // want `time.Sleep outside a synctest bubble`
}
