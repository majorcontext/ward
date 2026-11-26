package passwordfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/ward/internal/gitcred/credtest"
	"github.com/majorcontext/ward/internal/gitcred/passwordfile"
)

func TestConformance(t *testing.T) {
	t.Parallel()
	credtest.Run(t, credtest.Impl{New: func(t *testing.T) credtest.Subject {
		file := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(file, []byte("s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return credtest.Subject{Credential: passwordfile.Credential{User: "bot", File: file}, User: "bot", Secret: "s3cret"}
	}})
}
