// Package dir publishes release artifacts to a directory, such as a mounted bucket.
package dir

import (
	"github.com/majorcontext/ward/internal/release/publish"
	"os"
	"path/filepath"
)

// Publisher copies files to To. It ignores the tag.
type Publisher struct{ To string }

func (p Publisher) Publish(_ string, files []string) error {
	if err := os.MkdirAll(p.To, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p.To, filepath.Base(f)), data, 0o755); err != nil {
			return err
		}
	}
	return nil
}

var _ publish.Publisher = Publisher{}
