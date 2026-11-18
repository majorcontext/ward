package mirror

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	reg        *prometheus.Registry
	pushes     *prometheus.CounterVec
	hooks      *prometheus.CounterVec
	seeds      *prometheus.CounterVec
	cloneBytes prometheus.Gauge
	overBudget prometheus.Gauge
}

func newMetrics(store Store, clock Clock) *metrics {
	m := &metrics{
		reg:        prometheus.NewRegistry(),
		pushes:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ward_mirror_pushes_total", Help: "Ref actions by mirror and result."}, []string{"mirror", "result"}),
		hooks:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ward_hook_events_total", Help: "Delivered events by result."}, []string{"result"}),
		seeds:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "ward_mirror_seeds_total", Help: "Seeds by mirror and result."}, []string{"mirror", "result"}),
		cloneBytes: prometheus.NewGauge(prometheus.GaugeOpts{Name: "ward_mirror_clone_bytes", Help: "Bytes of the clone cache."}),
		overBudget: prometheus.NewGauge(prometheus.GaugeOpts{Name: "ward_mirror_disk_over_budget", Help: "1 while the clone cache is over its budget."}),
	}
	m.reg.MustRegister(m.pushes, m.hooks, m.seeds, m.cloneBytes, m.overBudget, &statsCollector{store: store, clock: clock})
	return m
}

// statsCollector reads the state store at each scrape, so no timer keeps the gauges current.
type statsCollector struct {
	store Store
	clock Clock
}

var (
	dirtyDesc   = prometheus.NewDesc("ward_mirror_dirty_refs", "Dirty refs by mirror.", []string{"mirror"}, nil)
	failingDesc = prometheus.NewDesc("ward_mirror_failing_refs", "Failing refs by mirror and class.", []string{"mirror", "class"}, nil)
	lagDesc     = prometheus.NewDesc("ward_mirror_lag_seconds", "Age of the oldest dirty ref by mirror.", []string{"mirror"}, nil)
)

func (*statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- dirtyDesc
	ch <- failingDesc
	ch <- lagDesc
}

func (c *statsCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats, err := c.store.Stats(ctx)
	if err != nil {
		return
	}
	now := c.clock.Now()
	for _, s := range stats {
		ch <- prometheus.MustNewConstMetric(dirtyDesc, prometheus.GaugeValue, float64(s.Dirty), s.Mirror)
		for class, n := range s.Failing {
			ch <- prometheus.MustNewConstMetric(failingDesc, prometheus.GaugeValue, float64(n), s.Mirror, string(class))
		}
		lag := 0.0
		if !s.OldestDirty.IsZero() {
			lag = now.Sub(s.OldestDirty).Seconds()
		}
		ch <- prometheus.MustNewConstMetric(lagDesc, prometheus.GaugeValue, lag, s.Mirror)
	}
}
