// Package passwordfile is the gitcred.Credential of a password or token that an operator keeps in a file. The askpass helper reads
// the file at each push, so a rotated file applies at once.
package passwordfile

import (
	"context"

	"github.com/majorcontext/ward/internal/gitcred"
	"github.com/majorcontext/ward/internal/gitremote"
)

// Credential names the user and the file of the password.
type Credential struct{ User, File string }

var _ gitcred.Credential = Credential{}

func (c Credential) Use(context.Context) (gitremote.Cred, func(), error) {
	return gitremote.Cred{AskUser: c.User, AskFile: c.File}, func() {}, nil
}
