package config

import (
	"strings"
	"testing"
)

func TestParseRequired(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ in, err string }{
		"a list":              {`[{"repo":"r","ref":"main","checks":["review"]}]`, ""},
		"a prefix":            {`[{"repo":"r","ref":"release/*","checks":["review"]}]`, ""},
		"empty":               {`[]`, ""},
		"null":                {`null`, "not null"},
		"not json":            {`main`, "must be a JSON list"},
		"unknown field":       {`[{"repo":"r","ref":"main","checks":["a"],"x":1}]`, "must be a JSON list"},
		"data after the list": {`[] []`, "data follows"},
		"no repo":             {`[{"ref":"main","checks":["a"]}]`, "each entry needs"},
		"bad ref":             {`[{"repo":"r","ref":"a*b","checks":["a"]}]`, "each entry needs"},
		"no checks":           {`[{"repo":"r","ref":"main","checks":[]}]`, "each entry needs"},
		"bad name":            {`[{"repo":"r","ref":"main","checks":["Review"]}]`, "check name"},
	} {
		_, err := parseRequired([]byte(c.in))
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", name, err, c.err)
		}
	}
}
