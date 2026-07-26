package telemetry

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// LogOption returns a zap.Option that counts every warn-or-worse entry into
// cigar_log_total{level,name} on its way to the real core. Apply it once, at
// the root logger, so every logger derived from it (With, Named) is counted:
//
//	log = log.WithOptions(m.LogOption())
//
// A nil *Metrics yields a pass-through option, so callers with no telemetry
// (e.g. `bot run`) can apply it unconditionally.
func (m *Metrics) LogOption() zap.Option {
	if m == nil {
		return zap.WrapCore(func(c zapcore.Core) zapcore.Core { return c })
	}
	return zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return &countingCore{Core: c, m: m}
	})
}

// countingCore is a proxy around the real zapcore.Core: it counts the entry,
// then delegates every decision and the actual writing to the wrapped core.
//
// It reports itself Enabled for counted levels even when the wrapped core is
// not, so cigar_log_total measures what the bot *experienced* rather than what
// the operator chose to print — running with --log-level error must not make
// the warning rate silently drop to zero. Entries the wrapped core rejects are
// counted and then discarded by it, so nothing extra reaches stdout.
type countingCore struct {
	zapcore.Core // Write/Sync/Enabled delegation
	m            *Metrics
}

func (c *countingCore) Enabled(l zapcore.Level) bool {
	return l >= logCountLevel || c.Core.Enabled(l)
}

// With must rewrap, or logger.With(...) would drop back to the bare core and
// stop counting.
func (c *countingCore) With(fields []zapcore.Field) zapcore.Core {
	return &countingCore{Core: c.Core.With(fields), m: c.m}
}

// Check is called once per log call, before any level filtering the wrapped
// core applies — which is why counting happens here and not in Write.
func (c *countingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if ent.Level >= logCountLevel {
		c.m.recordLog(ent.Level, ent.LoggerName)
	}
	return c.Core.Check(ent, ce)
}

// recordLog counts one entry. Unnamed loggers (the root one) are labelled
// "root" so the label is never empty.
func (m *Metrics) recordLog(level zapcore.Level, name string) {
	if name == "" {
		name = "root"
	}
	m.logCalls.WithLabelValues(level.String(), name).Inc()
}
