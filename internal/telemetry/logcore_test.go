package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// newTestLogger returns a logger writing to an observer at the given level,
// wrapped by the counting core under test.
func newTestLogger(m *Metrics, lvl zapcore.Level) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(lvl)
	return zap.New(core).WithOptions(m.LogOption()), logs
}

func TestLogOptionCountsByLevelAndName(t *testing.T) {
	m := New()
	log, logs := newTestLogger(m, zapcore.DebugLevel)

	log.Debug("noise")
	log.Info("noise")
	log.Warn("root warning")
	log.Named("webhook").Warn("queue nearly full")
	log.Named("webhook").Error("queue full, dropping event")
	log.Named("gitlab").Error("api call failed")

	tests := []struct {
		level, name string
		want        float64
	}{
		{"warn", "root", 1},
		{"warn", "webhook", 1},
		{"error", "webhook", 1},
		{"error", "gitlab", 1},
	}
	for _, tt := range tests {
		if got := testutil.ToFloat64(m.logCalls.WithLabelValues(tt.level, tt.name)); got != tt.want {
			t.Errorf("cigar_log_total{level=%q,name=%q} = %v, want %v", tt.level, tt.name, got, tt.want)
		}
	}

	// Debug and info are not counted: 4 series, one per case above.
	if got := testutil.CollectAndCount(m.logCalls); got != len(tests) {
		t.Errorf("counted series = %d, want %d (debug/info must not be counted)", got, len(tests))
	}

	// The proxy must not swallow or duplicate output.
	if got := logs.Len(); got != 6 {
		t.Errorf("emitted entries = %d, want 6", got)
	}
}

// The counter must reflect what the bot experienced, not what the operator
// chose to print: a warning is counted even when the log level hides it.
func TestLogOptionCountsBelowConfiguredLevel(t *testing.T) {
	m := New()
	log, logs := newTestLogger(m, zapcore.ErrorLevel)

	log.Named("worker").Warn("worker queue nearly full")

	if got := testutil.ToFloat64(m.logCalls.WithLabelValues("warn", "worker")); got != 1 {
		t.Errorf("warn count = %v, want 1 (must count below the configured level)", got)
	}
	if got := logs.Len(); got != 0 {
		t.Errorf("emitted %d entries, want 0 — the wrapped core must still filter", got)
	}
}

// logger.With() derives a new core; the proxy has to survive it.
func TestLogOptionSurvivesWith(t *testing.T) {
	m := New()
	log, _ := newTestLogger(m, zapcore.DebugLevel)

	log.Named("worker").With(zap.Int64("pipeline_id", 7)).Error("process pipeline failed")

	if got := testutil.ToFloat64(m.logCalls.WithLabelValues("error", "worker")); got != 1 {
		t.Errorf("error count after With = %v, want 1", got)
	}
}

func TestLogOptionNilMetrics(t *testing.T) {
	var m *Metrics
	core, logs := observer.New(zapcore.DebugLevel)
	log := zap.New(core).WithOptions(m.LogOption()) // must not panic

	log.Warn("still logged")

	if got := logs.Len(); got != 1 {
		t.Errorf("emitted %d entries, want 1", got)
	}
}
