package check_test

import (
	"strings"
	"testing"

	"github.com/majorcontext/ward/internal/check"
)

func TestParseFile(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 41)
	tests := []struct {
		name, in, wantErr string
		wantTimeout       int
	}{
		{"default timeout", `{"checks":[{"name":"unit","run":["go","test"]}]}`, "", 1800},
		{"explicit timeout", `{"checks":[{"name":"unit","run":["x"],"timeout_seconds":3600}]}`, "", 3600},
		{"no checks", `{"checks":[]}`, "", 0},
		{"unknown field", `{"checks":[],"extra":1}`, "does not parse", 0},
		{"unknown check field", `{"checks":[{"name":"a","run":["x"],"env":{}}]}`, "does not parse", 0},
		{"not json", `checks`, "does not parse", 0},
		{"data after the object", `{"checks":[]} {}`, "does not parse", 0},
		{"the file is null", `null`, "does not parse", 0},
		{"checks is null", `{"checks":null}`, "does not parse", 0},
		{"a null check", `{"checks":[null]}`, "does not parse", 0},
		{"a check that is no object", `{"checks":[1]}`, "does not parse", 0},
		{"checks is no list", `{"checks":{}}`, "does not parse", 0},
		{"a null in run", `{"checks":[{"name":"a","run":["x",null]}]}`, "run must be", 0},
		{"a number in run", `{"checks":[{"name":"a","run":[1]}]}`, "does not parse", 0},
		{"a null timeout", `{"checks":[{"name":"a","run":["x"],"timeout_seconds":null}]}`, "timeout_seconds", 0},
		{"comments and trailing commas", "// the checks\n{\n\t/* one */ \"checks\": [\n\t\t{\"name\": \"unit\", \"run\": [\"make\", \"test\",], \"timeout_seconds\": 60,}, // last\n\t],\n}\n", "", 60},
		{"a block comment does not hide trailing text", `{"checks":[]} /* c */ x`, "does not parse", 0},
		{"a line comment does not hide trailing text", "{\"checks\":[]} // c\nx", "does not parse", 0},
		{"an unterminated block comment", `{"checks":[]} /* c`, "does not parse", 0},
		{"a missing comma", `{"checks":[{"name":"a" "run":["x"]}]}`, "does not parse", 0},
		{"text after the object", `{"checks":[]} garbage`, "does not parse", 0},
		{"a closing brace after the object", `{"checks":[]}}`, "does not parse", 0},
		{"white space after the object", "{\"checks\":[]}\n\n", "", 0},
		{"name charset", `{"checks":[{"name":"Unit","run":["x"]}]}`, "name must be", 0},
		{"name length", `{"checks":[{"name":"` + long + `","run":["x"]}]}`, "name must be", 0},
		{"name repeats", `{"checks":[{"name":"a","run":["x"]},{"name":"a","run":["y"]}]}`, "name repeats", 0},
		{"empty run", `{"checks":[{"name":"a","run":[]}]}`, "run must be", 0},
		{"unknown key", `{"checks":[{"name":"a","run":["x"],"key":"branch"}]}`, "key must be", 0},
		{"null key", `{"checks":[{"name":"a","run":["x"],"key":null}]}`, "key must be", 0},
		{"timeout zero", `{"checks":[{"name":"a","run":["x"],"timeout_seconds":0}]}`, "timeout_seconds", 0},
		{"timeout too long", `{"checks":[{"name":"a","run":["x"],"timeout_seconds":3601}]}`, "timeout_seconds", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := check.ParseFile([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) > 0 && got[0].TimeoutSeconds != tt.wantTimeout {
				t.Fatalf("timeout = %d, want %d", got[0].TimeoutSeconds, tt.wantTimeout)
			}
		})
	}
}

func TestHashIsTheSHA256OfTheCompactObject(t *testing.T) {
	t.Parallel()
	c := check.Check{Name: "unit", Run: []string{"go", "test", "./..."}, TimeoutSeconds: 1800}
	const want = "e96efb8644b30bf8e97e4c69568e7c64b00173d095c4e85942f0e210a4b561a6"
	if got := c.Hash(); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
}

func TestHuJSONFileHasTheChecksAndTheHashOfItsPlainJSONForm(t *testing.T) {
	t.Parallel()
	hu, err := check.ParseFile([]byte("{\n// unit tests\n\"checks\": [{\"name\": \"unit\", /* argv */ \"run\": [\"make\", \"test\",],},],\n}\n"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := check.ParseFile([]byte(`{"checks":[{"name":"unit","run":["make","test"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(hu) != 1 || len(plain) != 1 || hu[0].Hash() != plain[0].Hash() || hu[0].TimeoutSeconds != 1800 {
		t.Fatalf("hujson %+v, plain %+v", hu, plain)
	}
}

func TestKeyChangesTheHashOnlyWhenItIsChange(t *testing.T) {
	t.Parallel()
	parse := func(key string) check.Check {
		cs, err := check.ParseFile([]byte(`{"checks":[{"name":"a","run":["x"]` + key + `}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return cs[0]
	}
	plain, tree, change := parse(""), parse(`,"key":"tree"`), parse(`,"key":"change"`)
	if plain.Hash() != tree.Hash() || plain.Hash() == change.Hash() {
		t.Fatalf("hashes: no key %s, tree %s, change %s", plain.Hash(), tree.Hash(), change.Hash())
	}
}
