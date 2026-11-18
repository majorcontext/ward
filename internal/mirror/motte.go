package mirror

import (
	"context"
	"crypto/ed25519"
	"strings"
	"time"

	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"
)

const (
	operatorLife = 60 * time.Second
	readLife     = 15 * time.Minute
	callBudget   = 30 * time.Second
)

// MotteConfig says how ward reaches motte and signs its grants.
type MotteConfig struct {
	URL string
	Kid string
	Key ed25519.PrivateKey
}

type motteSigner struct {
	cfg   MotteConfig
	clock Clock
}

func (m motteSigner) mint(scope grant.Scope, repo string, life time.Duration) (string, error) {
	return grant.Signer{Kid: m.cfg.Kid, Key: m.cfg.Key}.Mint(grant.Claims{Scope: scope, Repo: repo, Subject: "ward", Exp: m.clock.Now().Add(life).Unix()})
}

func (m motteSigner) client(scope grant.Scope, repo string, life time.Duration) *client.Client {
	return client.New(m.cfg.URL, func(context.Context) (string, error) { return m.mint(scope, repo, life) }, client.WithBudget(callBudget))
}

func (m motteSigner) operator() *client.Client {
	return m.client(grant.ScopeOperator, "", operatorLife)
}

func (m motteSigner) reader(repo string) *client.Client {
	return m.client(grant.ScopeRead, repo, operatorLife)
}

// gitURL is the smart HTTP URL of a repository text <prefix>:<path>.
func (m motteSigner) gitURL(repo string) string {
	prefix, path, _ := strings.Cut(repo, ":")
	return strings.TrimRight(m.cfg.URL, "/") + "/git/" + prefix + "/" + path + ".git"
}

// gitCreds returns the header of a read grant that lives long enough for one git run.
func (m motteSigner) gitCreds(repo string) (gitCreds, error) {
	tok, err := m.mint(grant.ScopeRead, repo, readLife)
	return gitCreds{Header: "Authorization: Bearer " + tok}, err
}
