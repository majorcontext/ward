// Package check holds the required checks of a ref, the results that a runner posts, and the delivery of check requests.
package check

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

const (
	// FilePath is the check file at the tip of a target ref.
	FilePath       = ".ward/checks.json"
	defaultTimeout = 1800
	maxTimeout     = 3600
)

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Check is one required check. The runner runs Run at the root of the tree.
type Check struct {
	Name           string   `json:"name"`
	Run            []string `json:"run"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// Hash is the SHA-256, in lowercase hex, of the compact JSON object of the check.
func (c Check) Hash() string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(c)
	sum := sha256.Sum256(bytes.TrimRight(b.Bytes(), "\n"))
	return hex.EncodeToString(sum[:])
}

// ParseFile decodes and checks the content of a check file. It rejects an unknown field.
func ParseFile(data []byte) ([]Check, error) {
	var in struct {
		Checks []struct {
			Name           string   `json:"name"`
			Run            []string `json:"run"`
			TimeoutSeconds *int     `json:"timeout_seconds"`
		} `json:"checks"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", FilePath, err)
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("%s does not parse: data follows the object", FilePath)
	}
	seen := map[string]bool{}
	out := make([]Check, 0, len(in.Checks))
	for _, c := range in.Checks {
		switch {
		case !nameRE.MatchString(c.Name):
			return nil, fmt.Errorf("check %q: name must be 1 to 40 characters of a-z 0-9 -", c.Name)
		case seen[c.Name]:
			return nil, fmt.Errorf("check %q: name repeats", c.Name)
		case len(c.Run) == 0:
			return nil, fmt.Errorf("check %q: run must be a non-empty list of strings", c.Name)
		}
		seen[c.Name] = true
		timeout := defaultTimeout
		if c.TimeoutSeconds != nil {
			timeout = *c.TimeoutSeconds
		}
		if timeout < 1 || timeout > maxTimeout {
			return nil, fmt.Errorf("check %q: timeout_seconds must be 1 to %d", c.Name, maxTimeout)
		}
		out = append(out, Check{Name: c.Name, Run: c.Run, TimeoutSeconds: timeout})
	}
	return out, nil
}

// ErrNoRunner means that no runner is configured for the repository.
var ErrNoRunner = errors.New("check: no runner for the repository")
