package mirror

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxPatterns     = 100
	defaultUsername = "x-access-token"
)

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Entry is one mirror of the configuration.
type Entry struct {
	Name           string     `json:"name"`
	Repo           string     `json:"repo"`
	Remote         string     `json:"remote"`
	Refs           []string   `json:"refs"`
	Username       string     `json:"username"`
	CredentialFile string     `json:"credential_file,omitempty"`
	GitHubApp      *GitHubApp `json:"github_app,omitempty"`
	PruneUnknown   bool       `json:"prune_unknown"`
	Seed           bool       `json:"seed"`
}

// ParseEntries decodes and checks the WARD_MIRRORS list. It rejects an unknown field.
func ParseEntries(raw []byte) ([]Entry, error) {
	var entries []Entry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("mirrors: %w", err)
	}
	seen := map[string]bool{}
	for i := range entries {
		e := &entries[i]
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("mirror %q: %w", e.Name, err)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf("mirror %q: name repeats", e.Name)
		}
		seen[e.Name] = true
		if e.GitHubApp != nil {
			abs, err := filepath.Abs(e.GitHubApp.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("mirror %q: github_app.key_file: %w", e.Name, err)
			}
			e.GitHubApp.KeyFile = abs
			e.Username = appUsername
			continue
		}
		abs, err := filepath.Abs(e.CredentialFile)
		if err != nil {
			return nil, fmt.Errorf("mirror %q: credential_file: %w", e.Name, err)
		}
		e.CredentialFile = abs
		if e.Username == "" {
			e.Username = defaultUsername
		}
	}
	return entries, nil
}

func (e Entry) validate() error {
	if !nameRE.MatchString(e.Name) {
		return errors.New("name must be 1 to 64 characters of a-z 0-9 -")
	}
	if prefix, path, ok := strings.Cut(e.Repo, ":"); !ok || prefix == "" || path == "" {
		return errors.New("repo must have the form <prefix>:<path>")
	}
	u, err := url.Parse(e.Remote)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("remote must be an https URL with no user information")
	}
	if len(e.Refs) == 0 || len(e.Refs) > maxPatterns {
		return fmt.Errorf("refs must hold 1 to %d patterns", maxPatterns)
	}
	for _, p := range e.Refs {
		if !validPattern(p) {
			return fmt.Errorf("invalid ref pattern %q", p)
		}
	}
	if e.GitHubApp != nil {
		return e.validateApp()
	}
	if e.CredentialFile == "" {
		return errors.New("credential_file or github_app is required")
	}
	if f, err := os.Open(e.CredentialFile); err != nil {
		return errors.New("credential_file is unreadable")
	} else {
		_ = f.Close()
	}
	return nil
}

func (e Entry) validateApp() error {
	a := e.GitHubApp
	switch {
	case e.CredentialFile != "" || e.Username != "":
		return errors.New("github_app excludes credential_file and username")
	case a.AppID <= 0 || a.InstallationID <= 0:
		return errors.New("github_app needs app_id and installation_id")
	}
	if _, err := readAppKey(a.KeyFile); err != nil {
		return fmt.Errorf("github_app.key_file: %w", err)
	}
	return nil
}

func validPattern(p string) bool {
	if p == "" || strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	return strings.Count(p, "*") == 0 || (strings.Count(p, "*") == 1 && strings.HasSuffix(p, "*"))
}

// Matches reports whether a pattern of e matches the stored ref name and the name has a git name.
func (e Entry) Matches(stored string) bool {
	if _, ok := GitRef(stored); !ok {
		return false
	}
	for _, p := range e.Refs {
		if prefix, wild := strings.CutSuffix(p, "*"); wild {
			if strings.HasPrefix(stored, prefix) {
				return true
			}
		} else if p == stored {
			return true
		}
	}
	return false
}

// GitRef maps a stored ref name to the git ref name. A stored name under refs/heads/ has none.
func GitRef(stored string) (string, bool) {
	switch {
	case stored == "" || strings.HasPrefix(stored, "refs/heads/"):
		return "", false
	case strings.HasPrefix(stored, "refs/"):
		return stored, true
	}
	return "refs/heads/" + stored, true
}

// StoredRef is the inverse of GitRef.
func StoredRef(git string) (string, bool) {
	if name, ok := strings.CutPrefix(git, "refs/heads/"); ok {
		return name, name != "" && !strings.HasPrefix(name, "refs/")
	}
	return git, strings.HasPrefix(git, "refs/")
}
