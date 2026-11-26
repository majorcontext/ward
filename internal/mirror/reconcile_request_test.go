package mirror

import (
	"runtime"
	"testing"
)

func TestRequestReconcileReadsAndActsInOneCriticalSection(t *testing.T) {
	t.Parallel()
	e := &Engine{entries: map[string]Entry{"m": {Name: "m"}}, workers: map[string]*worker{}, pending: map[string]bool{}}
	w := newWorker(e, e.entries["m"])
	e.workers["m"] = w
	w.mu.Lock()
	done := make(chan struct{})
	go func() { e.RequestReconcile("m"); close(done) }()
	held := 0
	for i := 0; held < 10000; i++ {
		if !e.mu.TryLock() {
			held++
		} else {
			held = 0
			e.mu.Unlock()
		}
		if i > 2e5 {
			t.Fatal("the request acts on the worker without the engine lock: a worker that starts now never sees it")
		}
		runtime.Gosched()
	}
	w.mu.Unlock()
	<-done
}
