// Package telemetry holds the bot's own operational metrics (prefix cigar_),
// exposed in Prometheus format on the ops server's /metrics endpoint.
//
// Everything lives on a dedicated registry (not the global default) so tests
// start from a clean slate and the standard go_*/process_* collectors can be
// added without leaking into other packages. All record methods are safe to
// call on a nil *Metrics — callers that have no telemetry (e.g. `bot run`) pass
// nil and pay nothing.
package telemetry

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap/zapcore"
)

// logCountLevel is the lowest level counted into cigar_log_total. Info and
// debug are ordinary traffic and would only add noise to an alert.
const logCountLevel = zapcore.WarnLevel

// Metrics owns the registry, collectors and the distinct-user set.
type Metrics struct {
	reg *prometheus.Registry

	webhookCalls *prometheus.CounterVec
	commandCalls *prometheus.CounterVec
	logCalls     *prometheus.CounterVec
	querySeconds prometheus.Counter
	usersActive  prometheus.Gauge

	mu    sync.Mutex
	users map[int64]struct{}
}

// New builds the metrics registry with the cigar_ collectors plus the standard
// Go runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		reg:   prometheus.NewRegistry(),
		users: make(map[int64]struct{}),
		webhookCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cigar_webhook_calls_total",
			Help: "Total GitLab webhook deliveries processed, by project and HTTP status.",
		}, []string{"project", "status"}),
		commandCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cigar_command_calls_total",
			Help: "Total interactive commands executed, by project and command.",
		}, []string{"project", "command"}),
		logCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cigar_log_total",
			Help: "Total warn-or-worse log entries, by level and logger name.",
		}, []string{"level", "name"}),
		querySeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cigar_prometheus_query_duration_seconds_total",
			Help: "Cumulative wall-clock seconds spent in Prometheus queries.",
		}),
		usersActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cigar_users_active",
			Help: "Distinct GitLab users seen since start (adoption).",
		}),
	}
	m.reg.MustRegister(
		m.webhookCalls,
		m.commandCalls,
		m.logCalls,
		m.querySeconds,
		m.usersActive,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RecordWebhook counts one webhook delivery for the project, tagged with the
// HTTP status returned. projectID is 0 when the request failed before the
// payload was parsed (e.g. an authentication failure returning 401).
func (m *Metrics) RecordWebhook(projectID int64, status int) {
	if m == nil {
		return
	}
	m.webhookCalls.WithLabelValues(
		strconv.FormatInt(projectID, 10),
		strconv.Itoa(status),
	).Inc()
}

// RecordCommand counts one executed command (kind) for the project.
func (m *Metrics) RecordCommand(projectID int64, kind string) {
	if m == nil {
		return
	}
	m.commandCalls.WithLabelValues(strconv.FormatInt(projectID, 10), kind).Inc()
}

// ObserveQuery adds the duration of one Prometheus query to the running total.
func (m *Metrics) ObserveQuery(d time.Duration) {
	if m == nil {
		return
	}
	m.querySeconds.Add(d.Seconds())
}

// RecordUser adds a GitLab user to the distinct set and updates the gauge.
// Non-positive IDs (absent/unknown user) are ignored.
func (m *Metrics) RecordUser(userID int64) {
	if m == nil || userID <= 0 {
		return
	}
	m.mu.Lock()
	if _, seen := m.users[userID]; !seen {
		m.users[userID] = struct{}{}
		m.usersActive.Set(float64(len(m.users)))
	}
	m.mu.Unlock()
}
