package mirror_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/mirror"
)

func TestMatchesAndGitRef(t *testing.T) {
	t.Parallel()
	e := mirror.Entry{Refs: []string{"main", "release/*", "refs/tags/*"}}
	all := mirror.Entry{Refs: []string{"*"}}
	tests := []struct {
		e     mirror.Entry
		ref   string
		match bool
		git   string
	}{
		{e, "main", true, "refs/heads/main"},
		{e, "release/1", true, "refs/heads/release/1"},
		{e, "release", false, "refs/heads/release"},
		{e, "refs/tags/v1", true, "refs/tags/v1"},
		{e, "refs/notes/x", false, "refs/notes/x"},
		{all, "anything", true, "refs/heads/anything"},
		{all, "refs/heads/x", false, ""},
	}
	for _, tt := range tests {
		if got := tt.e.Matches(tt.ref); got != tt.match {
			t.Errorf("Matches(%q) = %v, want %v", tt.ref, got, tt.match)
		}
		if g, _ := mirror.GitRef(tt.ref); g != tt.git {
			t.Errorf("GitRef(%q) = %q, want %q", tt.ref, g, tt.git)
		}
	}
	for git, want := range map[string]string{"refs/heads/a/b": "a/b", "refs/tags/t": "refs/tags/t"} {
		if got, ok := mirror.StoredRef(git); !ok || got != want {
			t.Errorf("StoredRef(%q) = %q, %v, want %q", git, got, ok, want)
		}
	}
	for _, git := range []string{"HEAD", "refs/heads/", "refs/heads/refs/x", "refs/heads/refs/tags/v1"} {
		if _, ok := mirror.StoredRef(git); ok {
			t.Errorf("StoredRef(%q) is ok", git)
		}
	}
}

func TestParseEntries(t *testing.T) {
	t.Parallel()
	cred := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(cred, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := func(mod string) string {
		s := `{"name":"a","repo":"native:r","remote":"https://git.example/r.git","refs":["main"],"credential_file":"` + cred + `"}`
		return "[" + strings.Replace(s, "}", mod+"}", 1) + "]"
	}
	if got, err := mirror.ParseEntries([]byte(entry(""))); err != nil || got[0].Username != "x-access-token" {
		t.Fatalf("valid entry: %v %v", got, err)
	}
	bad := map[string]string{
		"http remote":     `[{"name":"a","repo":"native:r","remote":"http://h/r","refs":["main"],"credential_file":"` + cred + `"}]`,
		"userinfo":        `[{"name":"a","repo":"native:r","remote":"https://u:p@h/r","refs":["main"],"credential_file":"` + cred + `"}]`,
		"empty refs":      strings.Replace(entry(""), `["main"]`, `[]`, 1),
		"mid wildcard":    strings.Replace(entry(""), `"main"`, `"a*b"`, 1),
		"bad name":        strings.Replace(entry(""), `"a"`, `"A_"`, 1),
		"missing cred":    strings.Replace(entry(""), cred, cred+"x", 1),
		"unknown field":   entry(`,"extra":1`),
		"repeated name":   "[" + strings.Trim(entry(""), "[]") + "," + strings.Trim(entry(""), "[]") + "]",
		"repo no prefix":  strings.Replace(entry(""), "native:r", "r", 1),
		"too many refs":   strings.Replace(entry(""), `["main"]`, "["+strings.Repeat(`"a",`, 100)+`"b"]`, 1),
		"not a list":      `{}`,
		"credential less": strings.Replace(entry(""), `"credential_file":"`+cred+`"`, `"credential_file":""`, 1),
	}
	for name, raw := range bad {
		if _, err := mirror.ParseEntries([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
