// Command leaklint fails when a tracked file or a commit message holds a term
// of the denylist. The denylist comes from the environment variable
// LEAK_DENYLIST, one term per line. The repository never holds the list.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

type hit struct {
	where string
	line  int
	term  string
}

func isWord(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// parseTerms returns the lowercase terms of a newline-separated list.
func parseTerms(list string) []string {
	var terms []string
	for _, t := range strings.Split(list, "\n") {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			terms = append(terms, t)
		}
	}
	return terms
}

// find returns the terms that occur in line as whole words, ignoring case. A
// boundary is needed only where the term itself starts or ends with a word
// character.
func find(line string, terms []string) []string {
	low := strings.ToLower(line)
	var out []string
	for _, t := range terms {
		for from := 0; ; {
			i := strings.Index(low[from:], t)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(t)
			from = start + 1
			if isWord(t[0]) && start > 0 && isWord(low[start-1]) {
				continue
			}
			if isWord(t[len(t)-1]) && end < len(low) && isWord(low[end]) {
				continue
			}
			out = append(out, t)
			break
		}
	}
	return out
}

func scan(where string, text []byte, terms []string) []hit {
	var hits []hit
	for i, line := range strings.Split(string(text), "\n") {
		for _, t := range find(line, terms) {
			hits = append(hits, hit{where, i + 1, t})
		}
	}
	return hits
}

func git(args ...string) ([]byte, error) {
	return exec.Command("git", args...).Output()
}

func run(list string, ci bool) ([]hit, error) {
	terms := parseTerms(list)
	if len(terms) == 0 {
		if ci {
			return nil, fmt.Errorf("LEAK_DENYLIST is empty")
		}
		fmt.Fprintln(os.Stderr, "leaklint: LEAK_DENYLIST is empty; skipped")
		return nil, nil
	}
	names, err := git("ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var hits []hit
	for i, name := range bytes.Split(bytes.TrimSuffix(names, []byte{0}), []byte{0}) {
		hits = append(hits, scan(fmt.Sprintf("tracked path %d of git ls-files", i+1), name, terms)...)
		data, err := os.ReadFile(string(name))
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		hits = append(hits, scan(string(name), data, terms)...)
	}
	log, err := git("log", "--no-merges", "--format=%H%n%an%n%ae%n%cn%n%ce%n%B%x01")
	if err != nil {
		return nil, err
	}
	for _, c := range bytes.Split(log, []byte{1}) {
		c = bytes.TrimLeft(c, "\n")
		if len(c) < 40 {
			continue
		}
		hits = append(hits, scan("commit "+string(c[:12]), c, terms)...)
	}
	return hits, nil
}

func main() {
	hits, err := run(os.Getenv("LEAK_DENYLIST"), os.Getenv("CI") != "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "leaklint:", err)
		os.Exit(1)
	}
	terms := parseTerms(os.Getenv("LEAK_DENYLIST"))
	for _, h := range hits {
		fmt.Printf("%s:%d: denied term #%d\n", h.where, h.line, slices.Index(terms, h.term)+1)
	}
	if len(hits) > 0 {
		os.Exit(1)
	}
}
