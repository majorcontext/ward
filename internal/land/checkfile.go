package land

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/check"
)

// fileError is a problem with the check file. It fails the land with land_invalid.
type fileError struct{ msg string }

func (e fileError) Error() string { return e.msg }

const (
	modeTree = "40000"
	modeFile = "100644"
	modeExec = "100755"
)

// requiredChecks reads the check file at the tip commit. A tip without the file has no required checks.
func requiredChecks(ctx context.Context, c *client.Client, tip string) ([]check.Check, error) {
	commit, err := c.Commit(ctx, tip)
	if err != nil {
		return nil, err
	}
	dir, ok, err := lookup(ctx, c, commit.Tree, ".ward", modeTree)
	if err != nil || !ok {
		return nil, err
	}
	file, ok, err := lookup(ctx, c, dir, "checks.json", modeFile, modeExec)
	if err != nil || !ok {
		return nil, err
	}
	obj, err := c.Object(ctx, file)
	if err != nil {
		return nil, err
	}
	checks, err := check.ParseFile(obj.Data)
	if err != nil {
		return nil, fileError{err.Error()}
	}
	return checks, nil
}

// lookup returns the SHA of the entry name of a tree. A missing entry is not an error. An entry of another mode is a fileError.
func lookup(ctx context.Context, c *client.Client, tree, name string, modes ...string) (string, bool, error) {
	obj, err := c.Object(ctx, tree)
	if err != nil {
		return "", false, err
	}
	data := obj.Data
	for len(data) > 0 {
		sp, nul := bytes.IndexByte(data, ' '), bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || len(data) < nul+21 {
			return "", false, errors.New("land: malformed tree")
		}
		mode, entry, sha := string(data[:sp]), string(data[sp+1:nul]), hex.EncodeToString(data[nul+1:nul+21])
		data = data[nul+21:]
		if entry != name {
			continue
		}
		for _, m := range modes {
			if mode == m {
				return sha, true, nil
			}
		}
		return "", false, fileError{fmt.Sprintf("%s is not a regular file", check.FilePath)}
	}
	return "", false, nil
}
