package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/majorcontext/ward/runnerproto"
)

// Dispatch modes of a Runner.
const (
	ModePush = "push"
	ModePull = "pull"
)

// Runner says how the check requests of one repository reach a runner. A push runner has a URL and a SecretFile: ward posts
// each request to it. A pull runner has Kids: each kid is a runner identity that polls ward for a lease.
type Runner struct {
	Repo         string   `json:"repo"`
	Mode         string   `json:"mode,omitempty"`
	URL          string   `json:"url,omitempty"`
	SecretFile   string   `json:"secret_file,omitempty"`
	Kids         []string `json:"kids,omitempty"`
	MinIsolation string   `json:"min_isolation,omitempty"`
	Env          string   `json:"env"`
}

// Pull reports whether runners fetch the requests of this repository.
func (r Runner) Pull() bool { return r.Mode == ModePull }

// ParseRunners decodes and checks the WARD_RUNNERS list. It rejects an unknown field.
func ParseRunners(raw []byte) ([]Runner, error) {
	var runners []Runner
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&runners); err != nil {
		return nil, fmt.Errorf("runners: %w", err)
	}
	seen := map[string]bool{}
	for i := range runners {
		r := &runners[i]
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("runner %q: %w", r.Repo, err)
		}
		if seen[r.Repo] {
			return nil, fmt.Errorf("runner %q: repo repeats", r.Repo)
		}
		seen[r.Repo] = true
		if r.Pull() {
			continue
		}
		abs, err := filepath.Abs(r.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("runner %q: secret_file: %w", r.Repo, err)
		}
		r.SecretFile = abs
	}
	return runners, nil
}

func (r *Runner) validate() error {
	if r.Repo == "" {
		return errors.New("repo is required")
	}
	switch r.Mode {
	case "", ModePush:
		r.Mode = ModePush
		if len(r.Kids) > 0 || r.MinIsolation != "" {
			return errors.New("kids and min_isolation belong to mode pull")
		}
		return r.validatePush()
	case ModePull:
		return r.validatePull()
	}
	return fmt.Errorf("mode must be %s or %s", ModePush, ModePull)
}

func (r *Runner) validatePull() error {
	if r.URL != "" || r.SecretFile != "" {
		return errors.New("url and secret_file belong to mode push")
	}
	if len(r.Kids) == 0 {
		return errors.New("kids is required for mode pull")
	}
	seen := map[string]bool{}
	for _, k := range r.Kids {
		if k == "" || seen[k] {
			return errors.New("kids must be non-empty and unique")
		}
		seen[k] = true
	}
	if r.MinIsolation == "" {
		r.MinIsolation = string(runnerproto.IsolationGVisor)
	}
	if runnerproto.Isolation(r.MinIsolation).Rank() == 0 {
		return fmt.Errorf("min_isolation must be %s or %s", runnerproto.IsolationGVisor, runnerproto.IsolationContainer)
	}
	return nil
}

func (r Runner) validatePush() error {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("url must be an http or https URL")
	}
	if _, err := r.Secret(); err != nil {
		return err
	}
	return nil
}

// Secret reads the key of the request signature. It reads the file at each call, so a rotated key applies at once.
func (r Runner) Secret() ([]byte, error) {
	raw, err := os.ReadFile(r.SecretFile)
	if err != nil {
		return nil, errors.New("secret_file is unreadable")
	}
	raw = bytes.TrimRight(raw, "\r\n")
	if len(raw) == 0 {
		return nil, errors.New("secret_file is empty")
	}
	return raw, nil
}
