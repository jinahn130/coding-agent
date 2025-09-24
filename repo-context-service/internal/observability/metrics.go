package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// RPC metrics
	rpcRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rpc_requests_total",
			Help: "Total number of RPC requests",
		},
		[]string{"service", "method", "code"},
	)

	rpcLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rpc_latency_seconds",
			Help:    "RPC request latency in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"service", "method"},
	)

	inFlightStreams = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "in_flight_streams",
			Help: "Number of in-flight streaming requests",
		},
		[]string{"service", "method"},
	)

	// Backend metrics
	backendLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "backend_latency_seconds",
			Help:    "Backend request latency in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		},
		[]string{"backend"},
	)

	// Cache metrics
	cacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_hits_total",
			Help: "Total number of cache hits",
		},
		[]string{"type"},
	)

	cacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cache_misses_total",
			Help: "Total number of cache misses",
		},
		[]string{"type"},
	)

	// Timing metrics
	timeToFirstHitMs = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "time_to_first_hit_ms",
			Help:    "Time to first search hit in milliseconds",
			Buckets: []float64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000},
		},
	)

	timeToSummaryMs = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "time_to_summary_ms",
			Help:    "Time to LLM summary completion in milliseconds",
			Buckets: []float64{100, 250, 500, 1000, 2500, 5000, 10000, 25000, 60000},
		},
	)

	// Upload metrics
	uploadRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "upload_requests_total",
			Help: "Total number of upload requests",
		},
		[]string{"source_type", "status"},
	)

	uploadSizeBytes = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "upload_size_bytes",
			Help:    "Upload size in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 2, 20), // 1KB to 512MB
		},
	)

	ingestionDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ingestion_duration_seconds",
			Help:    "Time taken for repository ingestion",
			Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600, 1200, 3600},
		},
	)

	// Search metrics
	searchResultsTotal = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "search_results_total",
			Help:    "Number of search results returned",
			Buckets: []float64{0, 1, 5, 10, 20, 50, 100},
		},
		[]string{"backend"},
	)

	embeddingRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "embedding_requests_total",
			Help: "Total number of embedding requests",
		},
		[]string{"model", "status"},
	)

	llmRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_requests_total",
			Help: "Total number of LLM requests",
		},
		[]string{"model", "status"},
	)
)

func init() {
	prometheus.MustRegister(
		rpcRequestsTotal,
		rpcLatencySeconds,
		inFlightStreams,
		backendLatencySeconds,
		cacheHitsTotal,
		cacheMissesTotal,
		timeToFirstHitMs,
		timeToSummaryMs,
		uploadRequestsTotal,
		uploadSizeBytes,
		ingestionDurationSeconds,
		searchResultsTotal,
		embeddingRequestsTotal,
		llmRequestsTotal,
	)
}

// Metrics provides methods for recording metrics
type Metrics struct{}

func NewMetrics() *Metrics {
	return &Metrics{}
}

// RPC metrics
func (m *Metrics) RecordRPCRequest(service, method string, code codes.Code, duration time.Duration) {
	rpcRequestsTotal.WithLabelValues(service, method, code.String()).Inc()
	rpcLatencySeconds.WithLabelValues(service, method).Observe(duration.Seconds())
}

func (m *Metrics) IncInFlightStreams(service, method string) {
	inFlightStreams.WithLabelValues(service, method).Inc()
}

func (m *Metrics) DecInFlightStreams(service, method string) {
	inFlightStreams.WithLabelValues(service, method).Dec()
}

// Backend metrics
func (m *Metrics) RecordBackendLatency(backend string, duration time.Duration) {
	backendLatencySeconds.WithLabelValues(backend).Observe(duration.Seconds())
}

// Cache metrics
func (m *Metrics) RecordCacheHit(cacheType string) {
	cacheHitsTotal.WithLabelValues(cacheType).Inc()
}

func (m *Metrics) RecordCacheMiss(cacheType string) {
	cacheMissesTotal.WithLabelValues(cacheType).Inc()
}

// Timing metrics
func (m *Metrics) RecordTimeToFirstHit(duration time.Duration) {
	timeToFirstHitMs.Observe(float64(duration.Nanoseconds()) / 1e6)
}

func (m *Metrics) RecordTimeToSummary(duration time.Duration) {
	timeToSummaryMs.Observe(float64(duration.Nanoseconds()) / 1e6)
}

// Upload metrics
func (m *Metrics) RecordUploadRequest(sourceType, status string) {
	uploadRequestsTotal.WithLabelValues(sourceType, status).Inc()
}

func (m *Metrics) RecordUploadSize(sizeBytes int64) {
	uploadSizeBytes.Observe(float64(sizeBytes))
}

func (m *Metrics) RecordIngestionDuration(duration time.Duration) {
	ingestionDurationSeconds.Observe(duration.Seconds())
}

// Search metrics
func (m *Metrics) RecordSearchResults(backend string, count int) {
	searchResultsTotal.WithLabelValues(backend).Observe(float64(count))
}

func (m *Metrics) RecordEmbeddingRequest(model, status string) {
	embeddingRequestsTotal.WithLabelValues(model, status).Inc()
}

func (m *Metrics) RecordLLMRequest(model, status string) {
	llmRequestsTotal.WithLabelValues(model, status).Inc()
}

// gRPC interceptors
func (m *Metrics) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		start := time.Now()
		service, method := splitMethodName(info.FullMethod)

		resp, err := handler(ctx, req)

		duration := time.Since(start)
		code := status.Code(err)
		m.RecordRPCRequest(service, method, code, duration)

		return resp, err
	}
}

func (m *Metrics) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		start := time.Now()
		service, method := splitMethodName(info.FullMethod)

		m.IncInFlightStreams(service, method)
		defer m.DecInFlightStreams(service, method)

		err := handler(srv, stream)

		duration := time.Since(start)
		code := status.Code(err)
		m.RecordRPCRequest(service, method, code, duration)

		return err
	}
}

// HTTP handler for metrics endpoint
func (m *Metrics) Handler() http.Handler {
	return promhttp.Handler()
}

// Helper functions
func splitMethodName(fullMethod string) (service, method string) {
	// fullMethod format: /package.service/method
	if len(fullMethod) < 2 {
		return "unknown", "unknown"
	}

	// Remove leading slash
	fullMethod = fullMethod[1:]

	// Split on last slash
	lastSlash := -1
	for i := len(fullMethod) - 1; i >= 0; i-- {
		if fullMethod[i] == '/' {
			lastSlash = i
			break
		}
	}

	if lastSlash == -1 {
		return "unknown", fullMethod
	}

	servicePath := fullMethod[:lastSlash]
	method = fullMethod[lastSlash+1:]

	// Extract service name from package.service
	lastDot := -1
	for i := len(servicePath) - 1; i >= 0; i-- {
		if servicePath[i] == '.' {
			lastDot = i
			break
		}
	}

	if lastDot == -1 {
		service = servicePath
	} else {
		service = servicePath[lastDot+1:]
	}

	return service, method
}

// Timer utility for measuring durations
type Timer struct {
	start time.Time
}

func StartTimer() *Timer {
	return &Timer{start: time.Now()}
}

func (t *Timer) Duration() time.Duration {
	return time.Since(t.start)
}

func (t *Timer) Milliseconds() float64 {
	return float64(t.Duration().Nanoseconds()) / 1e6
}