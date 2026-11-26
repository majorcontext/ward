package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/majorcontext/motte/client"
)

// maxWalk is the most commits that the deprecated merge base walks on each side.
const maxWalk = 10000

// ErrNoMergeBase means that the first-parent chain of the branch never meets the first-parent chain of the target ref.
var ErrNoMergeBase = errors.New("land: the branch has no merge base with the ref")

// baseOf returns the base of a land. A land that carries its base uses it. A land without one comes from a caller that
// predates the field, and ward finds the base itself.
//
// Deprecated: a later release removes the walk, after the callers pin a `ward land` that sends the base.
func (s *Service) baseOf(ctx context.Context, c *client.Client, l *Land, tip, head string) (string, error) {
	if l.Base != "" {
		return l.Base, nil
	}
	s.log.Warn("land without a base: ward finds the merge base itself, a deprecated path that a later release removes; use a ward land that sends the base", "land", l.ID)
	return mergeBase(ctx, c, tip, head)
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
