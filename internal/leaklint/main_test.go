package main

import (
	"reflect"
	"testing"
)

func TestFind(t *testing.T) {
	terms := parseTerms("Foo\nbar-baz\n198.51.\n\n")
	cases := []struct {
		line string
		want []string
	}{
		{"a FOO here", []string{"foo"}},
		{"food, foo_x, xfoo", []string{"foo"}},
		{"foobar and infoo", nil},
		{"use Bar-Baz now", []string{"bar-baz"}},
		{"addr 198.51.0.1", []string{"198.51."}},
		{"foo foo", []string{"foo"}},
	}
	for _, c := range cases {
		if got := find(c.line, terms); !reflect.DeepEqual(got, c.want) {
			t.Errorf("find(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestRunEmptyList(t *testing.T) {
	if _, err := run("", true); err == nil {
		t.Error("empty list passed in CI")
	}
	if _, err := run(" \n", false); err != nil {
		t.Errorf("empty list failed outside CI: %v", err)
	}
}
