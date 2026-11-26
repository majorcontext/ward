package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/majorcontext/ward/runnerproto"
)

// ModePull is the one dispatch mode. An entry may name it, so a list written for an earlier version still loads.
const ModePull = "pull"

// Runner names the runners that serve the check requests of one repository. Each kid is a runner identity that polls ward for
// a lease.
type Runner struct {
	Repo         string   `json:"repo"`
	Mode         string   `json:"mode,omitempty"`
	Kids         []string `json:"kids"`
	MinIsolation string   `json:"min_isolation,omitempty"`
	Env          string   `json:"env"`
}

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
	}
	return runners, nil
}

func (r *Runner) validate() error {
	if r.Repo == "" {
		return errors.New("repo is required")
	}
	if r.Mode != "" && r.Mode != ModePull {
		return errors.New("mode must be pull or absent")
	}
	if len(r.Kids) == 0 {
		return errors.New("kids is required")
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
