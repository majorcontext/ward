package fakeremote_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/gitremote"
	"github.com/majorcontext/ward/internal/gitremote/fakeremote"
	"github.com/majorcontext/ward/internal/gitremote/remotetest"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	remotetest.Run(t, remotetest.Impl{New: func(*testing.T) (gitremote.Remote, string) { return fakeremote.New(), "fake://remote" }})
}
