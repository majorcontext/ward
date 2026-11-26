// Package gitcred is the seam between the mirror and the source of the password that it gives to a git remote. The mirror owns
// the interface. passwordfile reads a file that an operator keeps, and forgeapp exchanges the key of a forge application for a
// short-lived token.
package gitcred

import (
	"context"

	"github.com/majorcontext/ward/internal/gitremote"
)

// Kind says why a credential could not be had.
type Kind int

const (
	// Local means that the failure is in ward: a key or a file that is not usable.
	Local Kind = iota
	// Unreachable means that the service that issues the credential did not answer or answered nonsense.
	Unreachable
	// Auth means that the service refused the key.
	Auth
)

// Error is a failure to get a credential. Its text never holds a token or a key.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Credential supplies the password of one remote.
type Credential interface {
	// Use returns the credential for one git run, and a function that removes any file that Use made. A caller calls the function
	// when the run ends. The password stays in a file that the askpass helper reads, never in an argument.
	Use(ctx context.Context) (gitremote.Cred, func(), error)
}
