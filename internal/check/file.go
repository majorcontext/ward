// Package check holds the required checks of a ref, the results that a runner posts, and the delivery of check requests.
package check

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type rawCheck struct {
	Name           string          `json:"name"`
	Run            []*string       `json:"run"`
	TimeoutSeconds json.RawMessage `json:"timeout_seconds"`
}

// isNull reports whether raw is the JSON literal null.
func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// ParseFile decodes and checks the content of a check file. It rejects an unknown field and every null, so that a
// file of the wrong shape never passes as an empty or a valid list.
func ParseFile(data []byte) ([]Check, error) {
	var in struct {
		Checks json.RawMessage `json:"checks"`
	}
	if isNull(data) {
		return nil, fmt.Errorf("%s does not parse: the file is null", FilePath)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("%s does not parse: %w", FilePath, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s does not parse: data follows the object", FilePath)
	}
	var entries []json.RawMessage
	if in.Checks != nil {
		if isNull(in.Checks) {
			return nil, fmt.Errorf("%s does not parse: checks is null", FilePath)
		}
		if err := json.Unmarshal(in.Checks, &entries); err != nil {
			return nil, fmt.Errorf("%s does not parse: checks must be a list", FilePath)
		}
	}
	seen := map[string]bool{}
	out := make([]Check, 0, len(entries))
	for _, raw := range entries {
		c, err := parseCheck(raw)
		if err != nil {
			return nil, err
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("check %q: name repeats", c.Name)
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out, nil
}

func parseCheck(raw json.RawMessage) (Check, error) {
	var c rawCheck
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if isNull(raw) || dec.Decode(&c) != nil {
		return Check{}, fmt.Errorf("%s does not parse: a check must be an object with name, run and timeout_seconds", FilePath)
	}
	run := make([]string, 0, len(c.Run))
	for _, r := range c.Run {
		if r == nil {
			return Check{}, fmt.Errorf("check %q: run must be a non-empty list of strings", c.Name)
		}
		run = append(run, *r)
	}
	timeout := defaultTimeout
	if c.TimeoutSeconds != nil {
		if err := json.Unmarshal(c.TimeoutSeconds, &timeout); err != nil || isNull(c.TimeoutSeconds) {
			return Check{}, fmt.Errorf("check %q: timeout_seconds must be 1 to %d", c.Name, maxTimeout)
		}
	}
	switch {
	case !nameRE.MatchString(c.Name):
		return Check{}, fmt.Errorf("check %q: name must be 1 to 40 characters of a-z 0-9 -", c.Name)
	case len(run) == 0:
		return Check{}, fmt.Errorf("check %q: run must be a non-empty list of strings", c.Name)
	case timeout < 1 || timeout > maxTimeout:
		return Check{}, fmt.Errorf("check %q: timeout_seconds must be 1 to %d", c.Name, maxTimeout)
	}
	return Check{Name: c.Name, Run: run, TimeoutSeconds: timeout}, nil
}

// ErrNoRunner means that no runner is configured for the repository.
var ErrNoRunner = errors.New("check: no runner for the repository")
