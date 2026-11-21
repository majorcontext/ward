package seams

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"
)

func readAllow(t *testing.T, name string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseAllowlist(name, raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// gapsAtMergeBase reads the gap file at the merge base with GATES_BASE_REF (default origin/main). A missing base
// skips the shrink check, unless SEAMS_REQUIRED is 1.
func gapsAtMergeBase(t *testing.T) (map[string]string, bool) {
	t.Helper()
	ref := os.Getenv("GATES_BASE_REF")
	if ref == "" {
		ref = "origin/main"
	}
	sha, err := exec.Command("git", "-C", "../..", "merge-base", "HEAD", ref).Output()
	if err != nil {
		if os.Getenv("SEAMS_REQUIRED") == "1" {
			t.Fatalf("find the merge base of HEAD and %s: %v", ref, err)
		}
		return nil, false
	}
	raw, err := exec.Command("git", "-C", "../..", "show", strings.TrimSpace(string(sha))+":testdata/seam-gaps.txt").Output()
	if err != nil {
		return nil, false
	}
	m, err := ParseAllowlist("seam-gaps.txt", raw)
	if err != nil {
		t.Fatal(err)
	}
	return m, true
}

func TestRepositorySeams(t *testing.T) {
	root := os.DirFS("../..")
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	seams, err := ParseTable(doc)
	if err != nil {
		t.Fatal(err)
	}
	atBase, haveBase := gapsAtMergeBase(t)
	hits, err := HostHits(m)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	all = append(all, CheckTable(m, seams)...)
	all = append(all, CheckGaps(seams, readAllow(t, "seam-gaps.txt"), atBase, haveBase)...)
	all = append(all, CheckSDKImports(m, ImplDirs(seams), readAllow(t, "provider-sdk-importers.txt"))...)
	all = append(all, CheckHosts(hits, readAllow(t, "provider-hosts.txt"))...)
	for _, v := range all {
		t.Error(v)
	}
}

const fixtureTable = `## Provider seams

| Seam | Interface | Suite | Implementations | Gap |
|---|---|---|---|---|
| kv | ` + "`kv.Store`" + ` | ` + "`kv/kvtest.Run`" + ` | ` + "`kv/mem`, `kv/disk`" + ` | |
| cred | none - concrete type | none - no interface | none | no interface yet |

## Next
`

func fixture() fstest.MapFS {
	suiteCall := "package disk_test\nimport \"example.test/m/kv/kvtest\"\nfunc TestX() { kvtest.Run(nil) }\n"
	return fstest.MapFS{
		"go.mod":               {Data: []byte("module example.test/m\n")},
		"kv/kv.go":             {Data: []byte("package kv\ntype Store interface{ Get() }\n")},
		"kv/kvtest/kvtest.go":  {Data: []byte("package kvtest\nfunc Run(t any) {}\n")},
		"kv/mem/mem.go":        {Data: []byte("package mem\n")},
		"kv/mem/mem_test.go":   {Data: []byte(strings.ReplaceAll(suiteCall, "disk_test", "mem_test"))},
		"kv/disk/disk.go":      {Data: []byte("package disk\n")},
		"kv/disk/disk_test.go": {Data: []byte(suiteCall)},
	}
}

func run(t *testing.T, fsys fstest.MapFS, table string) []string {
	t.Helper()
	m, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	seams, err := ParseTable([]byte(table))
	if err != nil {
		t.Fatal(err)
	}
	out := CheckTable(m, seams)
	out = append(out, CheckSDKImports(m, ImplDirs(seams), nil)...)
	hits, err := HostHits(m)
	if err != nil {
		t.Fatal(err)
	}
	return append(out, CheckHosts(hits, nil)...)
}

func TestCheckSeams(t *testing.T) {
	file := func(src string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(src)} }
	cases := []struct {
		name   string
		mutate func(fstest.MapFS)
		table  string
		want   string
	}{
		{"clean", func(fstest.MapFS) {}, fixtureTable, ""},
		{"missing interface", func(f fstest.MapFS) { f["kv/kv.go"] = file("package kv\n") }, fixtureTable, "kv: interface kv.Store is not declared"},
		{"duplicate implementation", func(fstest.MapFS) {}, strings.Replace(fixtureTable, "`kv/mem`, `kv/disk`", "`kv/mem`, `kv/mem`", 1), "kv: implementation kv/mem is listed twice"},
		{"implementation without suite call", func(f fstest.MapFS) { f["kv/disk/disk_test.go"] = file("package disk\n") }, fixtureTable, "kv: implementation kv/disk has no test that calls kv/kvtest.Run"},
		{"suite call outside a test function", func(f fstest.MapFS) {
			f["kv/disk/disk_test.go"] = file("package disk\nimport \"example.test/m/kv/kvtest\"\nfunc helper() { kvtest.Run() }\n")
		}, fixtureTable, "kv: implementation kv/disk has no test that calls kv/kvtest.Run"},
		{"unlisted subpackage", func(f fstest.MapFS) { f["kv/s3/s3.go"] = file("package s3\n") }, fixtureTable, "kv: package kv/s3 sits under kv and is not in the implementations"},
		{"unlisted nested subpackage", func(f fstest.MapFS) { f["kv/p/s3/s3.go"] = file("package s3\n") }, fixtureTable, "kv: package kv/p/s3 sits under kv and is not in the implementations"},
		{"unlisted assertion", func(f fstest.MapFS) {
			f["other/o.go"] = file("package other\nimport \"example.test/m/kv\"\nvar _ kv.Store = nil\n")
		}, fixtureTable, "kv: package other asserts kv.Store and is not in the implementations"},
		{"missing table row", func(fstest.MapFS) {}, strings.Replace(fixtureTable, ", `kv/disk`", "", 1), "kv: has 1 implementations, want at least 2, or a gap reason"},
		{"SDK import outside an implementation", func(f fstest.MapFS) {
			f["svc/s.go"] = file("package svc\nimport _ \"github.com/aws/aws-sdk-go-v2/service/s3\"\n")
		}, fixtureTable, "svc imports github.com/aws/aws-sdk-go-v2/service/s3; only an implementation package of a seam or cmd/ may"},
		{"SDK import in an implementation and cmd", func(f fstest.MapFS) {
			f["kv/mem/m2.go"] = file("package mem\nimport _ \"cloud.google.com/go/storage\"\n")
			f["cmd/x/main.go"] = file("package main\nimport _ \"google.golang.org/api/option\"\n")
		}, fixtureTable, ""},
		{"host literal", func(f fstest.MapFS) { f["svc/s.go"] = file("package svc\nvar u = \"https://ghcr.io/x\"\n") }, fixtureTable, "svc/s.go names ghcr.io; only a file of the host allowlist may"},
		{"module path literal", func(f fstest.MapFS) { f["svc/s.go"] = file("package svc\nvar u = \"example.test/m/x\"\n") }, fixtureTable, ""},
		{"host in chart", func(f fstest.MapFS) { f["charts/c/values.yaml"] = file("image: ghcr.io/x\n") }, fixtureTable, "charts/c/values.yaml names ghcr.io; only a file of the host allowlist may"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := fixture()
			c.mutate(f)
			got := strings.Join(run(t, f, c.table), "\n")
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCheckAllowlistsAndGaps(t *testing.T) {
	m, err := Load(fixture())
	if err != nil {
		t.Fatal(err)
	}
	seams, _ := ParseTable([]byte(fixtureTable))
	got := strings.Join(CheckSDKImports(m, ImplDirs(seams), map[string]string{"svc": "r", "kv/mem": "r"}), "\n")
	if !strings.Contains(got, "stale; delete it: svc") || !strings.Contains(got, "stale; delete it: kv/mem") {
		t.Errorf("stale SDK entries not reported: %q", got)
	}
	got = strings.Join(CheckHosts(nil, map[string]string{".github/workflows/*.yml": "r"}), "\n")
	if !strings.Contains(got, "stale; delete it: .github/workflows/*.yml") {
		t.Errorf("stale host entry not reported: %q", got)
	}
	file := map[string]string{"old": "r", "fresh": "r"}
	got = strings.Join(CheckGaps(seams, file, map[string]string{"old": "r"}, true), "\n")
	for _, want := range []string{"gap row is not in the gap file; the gap list only shrinks: cred", "no gap row in the table; delete it: fresh", "new against the merge base; the gap list only shrinks: fresh"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}
