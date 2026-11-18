package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Runner says where ward sends the check requests of one repository.
type Runner struct {
	Repo       string `json:"repo"`
	URL        string `json:"url"`
	SecretFile string `json:"secret_file"`
	Env        string `json:"env"`
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
		abs, err := filepath.Abs(r.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("runner %q: secret_file: %w", r.Repo, err)
		}
		r.SecretFile = abs
	}
	return runners, nil
}

func (r Runner) validate() error {
	if r.Repo == "" {
		return errors.New("repo is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("url must be an http or https URL")
	}
	if _, err := r.secret(); err != nil {
		return err
	}
	return nil
}

func (r Runner) secret() ([]byte, error) {
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
