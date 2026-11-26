package main

import (
	"sync"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	landSpans = tracetest.NewInMemoryExporter()
	traceOnce sync.Once
)

func traceInMemory() {
	traceOnce.Do(func() { otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(landSpans))) })
}

// checkLandSpans fails the test unless the land with id l recorded its request, queue and step spans.
func (h *harness) checkLandSpans(l string) {
	h.t.Helper()
	h.waitFor("the span of the run", func() bool {
		for _, s := range landSpans.GetSpans() {
			for _, kv := range s.Attributes {
				if s.Name == "ward.land.train" && kv.Key == "land_id" && kv.Value.AsString() == l {
					return true
				}
			}
		}
		return false
	})
	var run, runTrace string
	for _, s := range landSpans.GetSpans() {
		for _, kv := range s.Attributes {
			if kv.Key == "land_id" && kv.Value.AsString() == l && s.Name == "ward.land.train" {
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
	for _, name := range []string{"ward.serve POST /v1/lands", "ward.land.enqueue", "ward.land.train"} {
		if _, ok := got[name]; !ok {
			h.t.Errorf("no span %q", name)
		}
	}
	for _, name := range []string{"ward.land.trial", "ward.land.send"} {
		if run == "" || got[name] != run {
			h.t.Errorf("span %q has parent %q, want the run %q", name, got[name], run)
		}
	}
}
