package mirror

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMaybeAskpassAnswersUserThenCredential(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cred")
	if err := os.WriteFile(file, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(askpassFileEnv, file)
	t.Setenv(askpassUserEnv, "bot")
	for prompt, want := range map[string]string{"Username for 'https://h': ": "bot\n", "Password for 'https://bot@h': ": "tok\n"} {
		var out bytes.Buffer
		if !MaybeAskpass([]string{prompt}, &out) || out.String() != want {
			t.Errorf("%q answered %q, want %q", prompt, out.String(), want)
		}
	}
	t.Setenv(askpassFileEnv, "")
	if MaybeAskpass([]string{"Username"}, &bytes.Buffer{}) {
		t.Error("askpass ran with no marker")
	}
}
