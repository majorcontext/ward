// Package github publishes release artifacts as assets of a GitHub release through the gh CLI.
package github

import (
	"errors"

	"github.com/majorcontext/ward/internal/release/publish"
)

var _ publish.Publisher = Publisher{}

// Publisher attaches files to the release of the tag. Run runs gh with args;
// GH_TOKEN and GH_REPO select the account and repository.
type Publisher struct {
	Run func(args ...string) error
}

func (p Publisher) Publish(tag string, files []string) error {
	if tag == "" {
		return errors.New("github needs a tag")
	}
	// Two jobs may create the release at once; the loser reads the winner.
	if p.Run("release", "view", tag) != nil {
		if err := p.Run("release", "create", tag, "--title", tag, "--notes", ""); err != nil {
			if err := p.Run("release", "view", tag); err != nil {
				return err
			}
		}
	}
	return p.Run(append([]string{"release", "upload", tag, "--clobber"}, files...)...)
}
