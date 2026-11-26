// Package credtest is the conformance suite of gitcred.Credential.
package credtest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/gitcred"
)

// Subject is a credential and what it must give.
type Subject struct {
	Credential gitcred.Credential
	User       string
	Secret     string
	// Temporary is true when Use makes a file that the release function removes. Otherwise the file belongs to the operator and
	// stays.
	Temporary bool
}

// Impl builds the subject under test.
type Impl struct {
	New func(t *testing.T) Subject
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the password file: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// Run runs the suite.
func Run(t *testing.T, impl Impl) {
	ctx := context.Background()
	t.Run("the file holds the secret for the user", func(t *testing.T) {
		s := impl.New(t)
		c, release, err := s.Credential.Use(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if c.AskUser != s.User || c.AskFile == "" || c.Header != "" {
			t.Fatalf("credential = %+v, want user %q and a file", c, s.User)
		}
		if got := read(t, c.AskFile); got != s.Secret {
			t.Errorf("the file holds %q, want %q", got, s.Secret)
		}
	})
	t.Run("release removes only what Use made", func(t *testing.T) {
		s := impl.New(t)
		c, release, err := s.Credential.Use(ctx)
		if err != nil {
			t.Fatal(err)
		}
		release()
		_, statErr := os.Stat(c.AskFile)
		if s.Temporary && statErr == nil {
			t.Error("the file of Use is still there after release")
		}
		if !s.Temporary && statErr != nil {
			t.Errorf("release removed the file of the operator: %v", statErr)
		}
	})
	t.Run("two runs are independent", func(t *testing.T) {
		s := impl.New(t)
		c1, release1, err := s.Credential.Use(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c2, release2, err := s.Credential.Use(ctx)
		if err != nil {
			t.Fatal(err)
		}
		release1()
		if got := read(t, c2.AskFile); got != s.Secret {
			t.Errorf("the second file holds %q after the first release (first %s)", got, c1.AskFile)
		}
		release2()
	})
}
