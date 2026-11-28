package github

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/release/publish"
	"github.com/majorcontext/ward/internal/release/publish/publishtest"
)

// fakeGH is a gh that keeps releases and their assets in memory.
type fakeGH map[string]map[string]string

func (g fakeGH) run(args ...string) error {
	if len(args) < 3 || args[0] != "release" {
		return errors.New("unexpected gh call")
	}
	tag := args[2]
	switch args[1] {
	case "view":
		if g[tag] == nil {
			return errors.New("release not found")
		}
	case "create":
		g[tag] = map[string]string{}
	case "upload":
		if g[tag] == nil {
			return errors.New("release not found")
		}
		for _, f := range args[4:] {
			data, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			g[tag][filepath.Base(f)] = string(data)
		}
	}
	return nil
}

func TestConformance(t *testing.T) {
	t.Parallel()
	publishtest.Run(t, func(t *testing.T) (publish.Publisher, func() map[string]string) {
		g := fakeGH{}
		return Publisher{Run: g.run}, func() map[string]string { return g["v1.2.3"] }
	})
}
