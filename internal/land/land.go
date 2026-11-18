// Package land lands a motte branch on a ref with a merge grant.
package land

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/majorcontext/motte/api"
	"github.com/majorcontext/motte/client"
	"github.com/majorcontext/motte/grant"
)

const (
	grantLife = 5 * time.Minute
	maxWalk   = 10000
)

// ErrNoMergeBase means that the first-parent chain of the branch never meets the first-parent chain of the target ref.
var ErrNoMergeBase = errors.New("land: the branch has no merge base with the ref")

// Config says how ward reaches motte and signs its grants.
type Config struct {
	URL string
	Kid string
	Key ed25519.PrivateKey
}

// Request names the branch to land and where.
type Request struct {
	Repo         string
	Ref          string
	Branch       string
	DeleteBranch bool
}

// Result is the answer of motte, plus whether ward deleted the branch.
type Result struct {
	api.LandResponse
	BranchDeleted bool
}

// Service lands branches.
type Service struct {
	cfg Config
	now func() time.Time
}

// New returns a service. A nil now means time.Now.
func New(cfg Config, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{cfg: cfg, now: now}
}

func (s *Service) client(scope grant.Scope, repo string) *client.Client {
	return client.New(s.cfg.URL, func(context.Context) (string, error) {
		return grant.Signer{Kid: s.cfg.Kid, Key: s.cfg.Key}.Mint(grant.Claims{Scope: scope, Repo: repo, Subject: "ward", Exp: s.now().Add(grantLife).Unix()})
	})
}

// Land lands the commits of Branch that Ref lacks. The base is the newest commit on the first-parent chain of the
// tip of Ref that is also on the first-parent chain of the branch.
func (s *Service) Land(ctx context.Context, req Request) (Result, error) {
	reader, merger := s.client(grant.ScopeRead, req.Repo), s.client(grant.ScopeMerge, req.Repo)
	branch, err := reader.Ref(ctx, req.Branch)
	if err != nil {
		return Result{}, err
	}
	tip, err := reader.Ref(ctx, req.Ref)
	if err != nil {
		return Result{}, err
	}
	base, err := mergeBase(ctx, reader, tip.Commit, branch.Commit)
	if err != nil {
		return Result{}, err
	}
	out, err := merger.Land(ctx, req.Repo, api.LandRequest{Ref: req.Ref, Base: base, Head: branch.Commit})
	if err != nil {
		return Result{}, err
	}
	res := Result{LandResponse: out}
	if req.DeleteBranch {
		res.BranchDeleted = merger.DeleteRef(ctx, req.Branch, branch.Generation) == nil
	}
	return res, nil
}

func firstParent(ctx context.Context, c *client.Client, sha string) (string, error) {
	obj, err := c.Object(ctx, sha)
	if err != nil {
		return "", err
	}
	if obj.Type != "commit" {
		return "", fmt.Errorf("%w: %s is not a commit", ErrNoMergeBase, sha)
	}
	head, _, _ := bytes.Cut(obj.Data, []byte("\n\n"))
	for line := range strings.Lines(string(head)) {
		if p, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "parent "); ok {
			return p, nil
		}
	}
	return "", nil
}

// mergeBase walks both first-parent chains one commit at a time. The first commit that one walk reaches and the other
// has already seen is the newest common commit.
func mergeBase(ctx context.Context, c *client.Client, tip, head string) (string, error) {
	seenTip, seenHead := map[string]bool{}, map[string]bool{}
	for n := 0; (tip != "" || head != "") && n < maxWalk; n++ {
		var err error
		if tip != "" {
			if seenHead[tip] {
				return tip, nil
			}
			seenTip[tip] = true
			if tip, err = firstParent(ctx, c, tip); err != nil {
				return "", err
			}
		}
		if head != "" {
			if seenTip[head] {
				return head, nil
			}
			seenHead[head] = true
			if head, err = firstParent(ctx, c, head); err != nil {
				return "", err
			}
		}
	}
	return "", ErrNoMergeBase
}
