package observability

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsReflectEventsWithoutPrivateLabels(t *testing.T) {
	m := New(func(context.Context) (int64, error) { return 7, nil })
	m.Jobs.WithLabelValues("accepted").Inc()
	m.Failed.Inc()
	m.Active.Inc()
	m.LLM(time.Second, 42, nil)
	m.IngestBytes.Add(123)
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	text := response.Body.String()
	for _, sample := range []string{`documentation_jobs_total{event="accepted"} 1`, `queued_worker_jobs 7`, `llm_generated_tokens_total 42`, `active_worker_jobs 1`, `archive_ingestion_bytes 123`} {
		if !strings.Contains(text, sample) {
			t.Errorf("missing sample %s", sample)
		}
	}
	if strings.Contains(text, "project_id") || strings.Contains(text, "job_id") {
		t.Fatal("private/high-cardinality labels exposed")
	}
}
func TestQueueFailureDoesNotPretendItIsEmpty(t *testing.T) {
	m := New(func(context.Context) (int64, error) { return 0, errors.New("database unavailable") })
	response := httptest.NewRecorder()
	m.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	text := response.Body.String()
	if !strings.Contains(text, "documentation_queue_scrape_success 0") || strings.Contains(text, "queued_worker_jobs 0") {
		t.Fatal("failed query reported empty queue")
	}
}
func TestWorkerHealthSeparatesLivenessAndReadiness(t *testing.T) {
	handler := HealthHandler(func(context.Context) error { return errors.New("database down") }, New(nil))
	for _, test := range []struct {
		path   string
		status int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/metrics", 200}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", test.path, nil))
		if response.Code != test.status {
			t.Fatalf("%s status=%d", test.path, response.Code)
		}
	}
}
