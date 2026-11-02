// Package metricstest captures metrics in memory so a test can assert on what
// a code path emitted.
package metricstest

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/metrics"
)

// Metric is one recorded call.
type Metric struct {
	Name  string
	Type  string // "c", "g", "h", "d" or "ms", as on the wire
	Value float64
	Tags  []string
}

// HasTag reports whether the metric carries tag exactly ("key:value").
func (m Metric) HasTag(tag string) bool { return slices.Contains(m.Tags, tag) }

// TagValue returns the value of the first tag named key, and whether there
// was one.
func (m Metric) TagValue(key string) (string, bool) {
	for _, t := range m.Tags {
		if v, ok := strings.CutPrefix(t, key+":"); ok {
			return v, true
		}
	}
	return "", false
}

// Recorder is a metrics.Client that keeps every call.
type Recorder struct {
	mu      sync.Mutex
	metrics []Metric
}

var _ metrics.Client = (*Recorder)(nil)

// Install makes a new Recorder the process-wide default for the length of the
// test and returns it.
//
// The default is process-wide, so a test using this must not run in parallel
// with another that emits. Nothing in this repository calls t.Parallel.
func Install(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{}
	t.Cleanup(metrics.SetDefault(r))
	return r
}

func (r *Recorder) add(name, typ string, v float64, tags []string) {
	var kept []string
	for _, t := range tags {
		// metrics.Tag returns "" for an empty value and the wire format
		// skips it; the recorder skips it too, so a test sees what an agent
		// would.
		if t != "" {
			kept = append(kept, t)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, Metric{Name: name, Type: typ, Value: v, Tags: kept})
}

func (r *Recorder) Count(name string, value int64, tags ...string) {
	r.add(name, "c", float64(value), tags)
}

func (r *Recorder) Gauge(name string, value float64, tags ...string) {
	r.add(name, "g", value, tags)
}

func (r *Recorder) Histogram(name string, value float64, tags ...string) {
	r.add(name, "h", value, tags)
}

func (r *Recorder) Distribution(name string, value float64, tags ...string) {
	r.add(name, "d", value, tags)
}

func (r *Recorder) Timing(name string, d time.Duration, tags ...string) {
	r.add(name, "ms", float64(d)/float64(time.Millisecond), tags)
}

func (r *Recorder) Close() error { return nil }

// All returns a copy of every recorded metric, in order.
func (r *Recorder) All() []Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Metric(nil), r.metrics...)
}

// Named returns every recorded metric called name, in order.
func (r *Recorder) Named(name string) []Metric {
	var out []Metric
	for _, m := range r.All() {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

// Total sums the values recorded under name. For a counter that always adds
// 1, it is how many times the call site fired.
func (r *Recorder) Total(name string) float64 {
	var sum float64
	for _, m := range r.Named(name) {
		sum += m.Value
	}
	return sum
}
