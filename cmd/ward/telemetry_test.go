package main

import (
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	landSpans = tracetest.NewInMemoryExporter()
	traceOnce sync.Once
)

func TestALandRecordsItsRequestQueueAndSteps(t *testing.T) {
	traceOnce.Do(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(landSpans))) })
	h := newLandHarness(t)
	p := h.startWard(wardOpts{noMirror: true})
	base := h.seedMain()
	h.branchFrom(base, "feature/x", "c.txt", "c\n")
	h.branchFrom(base, "main", "b.txt", "b\n")
	h.protectMain()
	l := h.postLand(p, "", "feature/x", nil)
	h.awaitLand(p, l.ID, "landed")
	h.waitFor("the span of the run", func() bool {
		for _, s := range landSpans.GetSpans() {
			for _, kv := range s.Attributes {
				if s.Name == "ward.land.run" && kv.Key == "land_id" && kv.Value.AsString() == l.ID {
					return true
				}
			}
		}
		return false
	})

	var run, runTrace string
	for _, s := range landSpans.GetSpans() {
		for _, kv := range s.Attributes {
			if kv.Key == "land_id" && kv.Value.AsString() == l.ID && s.Name == "ward.land.run" {
				run, runTrace = s.SpanContext.SpanID().String(), s.SpanContext.TraceID().String()
			}
		}
	}
	got := map[string]string{}
	for _, s := range landSpans.GetSpans() {
		if s.SpanContext.TraceID().String() == runTrace || s.Name == "ward.land.enqueue" || s.Name == "ward.serve POST /v1/lands" {
			got[s.Name] = s.Parent.SpanID().String()
		}
	}
	for _, name := range []string{"ward.serve POST /v1/lands", "ward.land.enqueue", "ward.land.run"} {
		if _, ok := got[name]; !ok {
			t.Errorf("no span %q", name)
		}
	}
	for _, name := range []string{"ward.land.prepare", "ward.land.checks", "ward.land.send"} {
		if run == "" || got[name] != run {
			t.Errorf("span %q has parent %q, want the run %q", name, got[name], run)
		}
	}
}
