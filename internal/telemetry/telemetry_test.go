package telemetry

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordWebhook(t *testing.T) {
	m := New()
	m.RecordWebhook(42, 200)
	m.RecordWebhook(42, 200)
	m.RecordWebhook(7, 200)
	m.RecordWebhook(0, 401) // auth failure, project unknown

	if got := testutil.ToFloat64(m.webhookCalls.WithLabelValues("42", "200")); got != 2 {
		t.Errorf("project 42 status 200 webhook calls = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.webhookCalls.WithLabelValues("7", "200")); got != 1 {
		t.Errorf("project 7 status 200 webhook calls = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.webhookCalls.WithLabelValues("0", "401")); got != 1 {
		t.Errorf("project 0 status 401 webhook calls = %v, want 1", got)
	}
}

func TestRecordCommand(t *testing.T) {
	m := New()
	m.RecordCommand(42, "advise")
	m.RecordCommand(42, "advise")
	m.RecordCommand(42, "help")

	if got := testutil.ToFloat64(m.commandCalls.WithLabelValues("42", "advise")); got != 2 {
		t.Errorf("advise calls = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.commandCalls.WithLabelValues("42", "help")); got != 1 {
		t.Errorf("help calls = %v, want 1", got)
	}
}

func TestObserveQuery(t *testing.T) {
	m := New()
	m.ObserveQuery(250 * time.Millisecond)
	m.ObserveQuery(750 * time.Millisecond)

	if got := testutil.ToFloat64(m.querySeconds); got != 1 {
		t.Errorf("query seconds total = %v, want 1", got)
	}
}

func TestRecordUserDistinct(t *testing.T) {
	m := New()
	m.RecordUser(1)
	m.RecordUser(1) // duplicate
	m.RecordUser(2)
	m.RecordUser(0)  // ignored
	m.RecordUser(-5) // ignored

	if got := testutil.ToFloat64(m.usersActive); got != 2 {
		t.Errorf("active users = %v, want 2", got)
	}
}

func TestNilMetricsNoPanic(t *testing.T) {
	var m *Metrics
	// None of these should panic on a nil receiver.
	m.RecordWebhook(1, 200)
	m.RecordCommand(1, "help")
	m.ObserveQuery(time.Second)
	m.RecordUser(1)
}

func TestHandlerExposesCigarMetrics(t *testing.T) {
	m := New()
	m.RecordWebhook(99, 200)
	m.RecordCommand(99, "advise") // label-vec metrics only emit once a series exists

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	body, _ := io.ReadAll(rec.Result().Body)
	out := string(body)
	if !strings.Contains(out, `cigar_webhook_calls_total{project="99",status="200"} 1`) {
		t.Errorf("metrics output missing webhook counter, got:\n%s", out)
	}
	for _, name := range []string{
		"cigar_command_calls_total",
		"cigar_prometheus_query_duration_seconds_total",
		"cigar_users_active",
	} {
		if !strings.Contains(out, name) {
			t.Errorf("metrics output missing %s", name)
		}
	}
}
