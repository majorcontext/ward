package mirror

import (
	"context"
	"errors"

	"github.com/majorcontext/ward/internal/gitcred"
	"github.com/majorcontext/ward/internal/gitcred/forgeapp"
	"github.com/majorcontext/ward/internal/gitcred/passwordfile"
)

// DefaultGitHubAPI is the API base URL of the GitHub App credential when the configuration names none.
const DefaultGitHubAPI = forgeapp.DefaultAPI

// GitHubApp is a GitHub App installation that supplies the remote credential.
type GitHubApp struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	KeyFile        string `json:"key_file"`
}

// credentialOf returns the credential of a mirror entry: a GitHub App when the entry has one, else its password file.
func credentialOf(en Entry, api, dir string, clock Clock) gitcred.Credential {
	if a := en.GitHubApp; a != nil {
		return forgeapp.New(forgeapp.App{AppID: a.AppID, InstallationID: a.InstallationID, KeyFile: a.KeyFile}, api, dir, clock)
	}
	return passwordfile.Credential{User: en.Username, File: en.CredentialFile}
}

// credClass maps a failure to get a credential to the class of a mirror failure.
func credClass(err error) Class {
	var ce *gitcred.Error
	if errors.As(err, &ce) {
		switch ce.Kind {
		case gitcred.Unreachable:
			return ClassRemoteUnreachable
		case gitcred.Auth:
			return ClassRemoteAuth
		}
	}
	return ClassLocal
}

// remoteCreds returns the credential of a mirror for one git run and the function that removes its token file.
func (e *Engine) remoteCreds(ctx context.Context, en Entry) (gitCreds, func(), error) {
	return e.creds[en.Name].Use(ctx)
}
