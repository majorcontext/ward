package fakerunner_test

import (
	"testing"

	"github.com/majorcontext/ward/internal/runner/fakerunner"
	"github.com/majorcontext/ward/runnerproto"
	"github.com/majorcontext/ward/runnerproto/runnertest"
)

func TestConformance(t *testing.T) {
	runnertest.Run(t, runnertest.Impl{New: func(_ *testing.T, e runnertest.Env) runnerproto.Runner {
		return fakerunner.Runner{Secret: e.Secret, WardURL: e.WardURL, Kid: e.Kid, Key: e.Key}
	}})
}
