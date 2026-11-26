package land

import (
	"context"
	"errors"

	"github.com/majorcontext/motte/client"

	"github.com/majorcontext/ward/internal/check"
)

// fileError is a problem with the check file. It fails the land with land_invalid.
type fileError struct{ msg string }

func (e fileError) Error() string { return e.msg }

// requiredChecks reads the check file at the tip commit. A tip without the file has no required checks.
func requiredChecks(ctx context.Context, c *client.Client, repo, tip string) ([]check.Check, error) {
	data, err := c.File(ctx, repo, tip, check.FilePath)
	if errors.Is(err, client.ErrPathNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	checks, err := check.ParseFile(data)
	if err != nil {
		return nil, fileError{err.Error()}
	}
	return checks, nil
}
