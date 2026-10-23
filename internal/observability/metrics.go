package observability

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry         *prometheus.Registry
	Jobs             *prometheus.CounterVec
	Failed           prometheus.Counter
	JobDuration      prometheus.Histogram
	AnalysisDuration prometheus.Histogram
	LLMRequests      *prometheus.CounterVec
	LLMDuration      prometheus.Histogram
	LLMTokens        prometheus.Counter
	Active           prometheus.Gauge
	IngestBytes      prometheus.Counter
	HTTPRequests     *prometheus.CounterVec
}

func New(queueCount func(context.Context) (int64, error)) *Metrics {
	r := prometheus.NewRegistry()
	durations := []float64{0.1, 1, 10, 60, 300, 900, 1800, 3600, 7200}
	m := &Metrics{Registry: r,
		Jobs:             prometheus.NewCounterVec(prometheus.CounterOpts{Name: "documentation_jobs_total", Help: "Job lifecycle events, labeled accepted/completed."}, []string{"event"}),
		Failed:           prometheus.NewCounter(prometheus.CounterOpts{Name: "documentation_jobs_failed_total", Help: "Worker jobs that failed."}),
		JobDuration:      prometheus.NewHistogram(prometheus.HistogramOpts{Name: "documentation_job_duration_seconds", Help: "Worker job processing duration including inference wait.", Buckets: durations}),
		AnalysisDuration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "repository_analysis_duration_seconds", Help: "Static analysis duration.", Buckets: []float64{0.001, 0.01, 0.1, 1, 5, 30, 120}}),
		LLMRequests:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llm_requests_total", Help: "AI provider HTTP attempts labeled success/error."}, []string{"outcome"}),
		LLMDuration:      prometheus.NewHistogram(prometheus.HistogramOpts{Name: "llm_request_duration_seconds", Help: "AI provider HTTP attempt duration.", Buckets: durations}),
		LLMTokens:        prometheus.NewCounter(prometheus.CounterOpts{Name: "llm_generated_tokens_total", Help: "Reported generated tokens for completed Ollama responses."}),
		Active:           prometheus.NewGauge(prometheus.GaugeOpts{Name: "active_worker_jobs", Help: "Currently processing jobs in this worker process."}),
		IngestBytes:      prometheus.NewCounter(prometheus.CounterOpts{Name: "archive_ingestion_bytes", Help: "Successfully stored upload bytes including duplicate uploads."}),
		HTTPRequests:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "documentation_http_requests_total", Help: "HTTP responses with fixed component and status labels."}, []string{"component", "status"}),
	}
	r.MustRegister(m.Jobs, m.Failed, m.JobDuration, m.AnalysisDuration, m.LLMRequests, m.LLMDuration, m.LLMTokens, m.Active, m.IngestBytes, m.HTTPRequests)
	r.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	if queueCount != nil {
		r.MustRegister(&queueCollector{count: queueCount, queued: prometheus.NewDesc("queued_worker_jobs", "Queued jobs in the shared database; count once, not summed across replicas.", nil, nil), up: prometheus.NewDesc("documentation_queue_scrape_success", "Whether the shared queue gauge query succeeded.", nil, nil)})
	}
	return m
}
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
func (m *Metrics) HTTP(component string, status int) {
	if m != nil {
		m.HTTPRequests.WithLabelValues(component, strconv.Itoa(status)).Inc()
	}
}

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseStatus) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseStatus) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (m *Metrics) Instrument(component string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &responseStatus{ResponseWriter: w}
		handler.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		m.HTTP(component, status)
	})
}
func (m *Metrics) LLM(duration time.Duration, tokens int, err error) {
	if m == nil {
		return
	}
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.LLMRequests.WithLabelValues(outcome).Inc()
	m.LLMDuration.Observe(duration.Seconds())
	if tokens > 0 {
		m.LLMTokens.Add(float64(tokens))
	}
}

type queueCollector struct {
	count      func(context.Context) (int64, error)
	queued, up *prometheus.Desc
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.queued; ch <- c.up }
func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	count, err := c.count(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.queued, prometheus.GaugeValue, float64(count))
}
func HealthHandler(ready func(context.Context) error, metrics *Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if ready != nil && ready(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", metrics.Handler())
	return metrics.Instrument("worker", mux)
}
