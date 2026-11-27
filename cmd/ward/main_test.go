package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/majorcontext/motte/bucket"
	metasqlite "github.com/majorcontext/motte/meta/sqlite"
	"github.com/majorcontext/motte/testserver"

	statesqlite "github.com/majorcontext/ward/internal/state/sqlite"
)

// askpassBin answers git like the ward binary does, but starts at once: compiling ward for a test costs more than the tests.
var askpassBin string

// bareTemplate is a bare repository that each remote copies, and metaTemplate is a migrated database of motte that each
// motte copies. Both are made once, because each test would otherwise start git or run the migrations for them.
var bareTemplate, workTemplate, metaTemplate string

// seededTB gives the first directory that testserver asks for the migrated database of motte.
type seededTB struct {
	testing.TB
	seeded bool
}

func (s *seededTB) TempDir() string {
	d := s.TB.TempDir()
	if !s.seeded {
		s.seeded = true
		b, err := os.ReadFile(metaTemplate)
		if err != nil || os.WriteFile(filepath.Join(d, "meta.db"), b, 0o644) != nil {
			s.Fatal("copy the database template", err)
		}
	}
	return d
}

// startMotte runs a motte whose metadata and objects live in memory. A database file makes each query lock files and each
// commit sync them, and a bucket file is synced too: both cost more than the work. The server configuration is an
// internal type, so reflection sets its stores.
func startMotte(t testing.TB) *testserver.Server {
	t.Helper()
	mem, err := metasqlite.Open(context.Background(), metasqlite.Memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	bkt := &memBucket{objs: map[string]memObject{}}
	start := reflect.ValueOf(testserver.StartLocalWith)
	change := reflect.MakeFunc(start.Type().In(1), func(args []reflect.Value) []reflect.Value {
		args[0].Elem().FieldByName("Meta").Set(reflect.ValueOf(mem))
		args[0].Elem().FieldByName("Bucket").Set(reflect.ValueOf(bkt))
		return nil
	})
	s := start.Call([]reflect.Value{reflect.ValueOf(&seededTB{TB: t}), change})[0].Interface().(*testserver.Server)
	s.Meta, s.Bucket = mem, bkt
	return s
}

// sharedMotte is the motte of the tests that need no mirror.
var sharedMotte *testserver.Server

// mainTB lets testserver start a server for the whole package: it has no test to hold the cleanup.
type mainTB struct {
	testing.TB
	dir string
}

func (mainTB) Helper() {}

func (b mainTB) TempDir() string {
	d, err := os.MkdirTemp(b.dir, "motte")
	if err != nil {
		panic(err)
	}
	return d
}

func (mainTB) Cleanup(func()) {}

func (mainTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

const askpassScript = `#!/bin/sh
case "$1" in
Username*) echo "$WARD_ASKPASS_USER" ;;
*) IFS= read -r secret <"$WARD_ASKPASS_FILE"; echo "$secret" ;;
esac
`

// stateTemplate is the migrated state database of ward that each harness copies.
var stateTemplate []byte

// memoryTemp points the temporary directories at memory when a gigabyte of it is free and a script can run there, so
// no commit of a database or a clone waits for a disk. The tests write scripts to their temporary directories.
func memoryTemp() {
	var st syscall.Statfs_t
	if syscall.Statfs("/dev/shm", &st) != nil || int64(st.Bavail)*int64(st.Bsize) < 1<<30 {
		return
	}
	dir, err := os.MkdirTemp("/dev/shm", "ward-probe")
	if err != nil {
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, "probe")
	if os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755) == nil && exec.Command(script).Run() == nil {
		_ = os.Setenv("TMPDIR", "/dev/shm")
	}
}

func TestMain(m *testing.M) {
	memoryTemp()
	debug.SetGCPercent(400)
	dir, err := os.MkdirTemp("", "ward-askpass")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bareTemplate, workTemplate = filepath.Join(dir, "bare.git"), filepath.Join(dir, "work")
	if err := os.Mkdir(workTemplate, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, args := range [][]string{{"init", "--bare", "-q", "--template=", bareTemplate}, {"-C", bareTemplate, "config", "http.receivepack", "true"}, {"init", "-q", "--template=", workTemplate}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			fmt.Fprintln(os.Stderr, "git", args, err, string(out))
			os.Exit(1)
		}
	}
	if err := os.Mkdir(filepath.Join(bareTemplate, "hooks"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stateDB, err := statesqlite.Open(context.Background(), filepath.Join(dir, "state.tmpl"))
	if err != nil || stateDB.Close() != nil {
		fmt.Fprintln(os.Stderr, "migrate the state template", err)
		os.Exit(1)
	}
	if stateTemplate, err = os.ReadFile(filepath.Join(dir, "state.tmpl")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	metaTemplate = filepath.Join(dir, "meta.tmpl")
	db, err := metasqlite.Open(context.Background(), metaTemplate)
	if err != nil || db.Close() != nil {
		fmt.Fprintln(os.Stderr, "migrate the database template", err)
		os.Exit(1)
	}
	sharedMotte = startMotte(mainTB{dir: dir})
	askpassBin = filepath.Join(dir, "askpass")
	if err := os.WriteFile(askpassBin, []byte(askpassScript), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Each test spawns many git processes. A crowd of them makes every test slow, so few run at once.
	_ = flag.Set("test.parallel", strconv.Itoa(min(runtime.NumCPU(), 8)))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type memObject struct {
	data    []byte
	version int
}

// memBucket is a bucket.Store in memory.
type memBucket struct {
	mu   sync.Mutex
	next int
	objs map[string]memObject
}

func (b *memBucket) Create(_ context.Context, key string, data []byte) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objs[key]; ok {
		return false, nil
	}
	b.next++
	b.objs[key] = memObject{bytes.Clone(data), b.next}
	return true, nil
}

func (b *memBucket) Get(ctx context.Context, key string) ([]byte, error) {
	return b.GetRange(ctx, key, 0, -1)
}

func (b *memBucket) GetRange(_ context.Context, key string, off, n int64) ([]byte, error) {
	b.mu.Lock()
	o, ok := b.objs[key]
	b.mu.Unlock()
	if !ok {
		return nil, bucket.ErrNotFound
	}
	end := int64(len(o.data))
	if n >= 0 {
		end = min(end, off+n)
	}
	if off >= end {
		return nil, nil
	}
	return bytes.Clone(o.data[off:end]), nil
}

func (b *memBucket) Version(_ context.Context, key string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objs[key]
	if !ok {
		return "", bucket.ErrNotFound
	}
	return strconv.Itoa(o.version), nil
}

func (b *memBucket) Delete(_ context.Context, key, version string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.objs[key]; ok {
		if strconv.Itoa(o.version) != version {
			return bucket.ErrChanged
		}
		delete(b.objs, key)
	}
	return nil
}
