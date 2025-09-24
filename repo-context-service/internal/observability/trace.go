package observability

import (
	"context"

	"google.golang.org/grpc"
)

const TracerName = "repo-context-service"

// Stub implementations for tracing - no OpenTelemetry dependency

type Tracer struct{}

type Span struct{}

type Attribute struct {
	Key   string
	Value interface{}
}

// NewNoOpTracer creates a tracer that does nothing
func NewNoOpTracer() *Tracer {
	return &Tracer{}
}

func NewTracer(serviceName, serviceVersion, otlpEndpoint string) (*Tracer, func(), error) {
	// Return a no-op tracer to avoid dependency conflicts
	return &Tracer{}, func() {}, nil
}

func (t *Tracer) Start(ctx context.Context, name string, opts ...interface{}) (context.Context, *Span) {
	return ctx, &Span{}
}

// Helper methods for common tracing patterns

func (t *Tracer) StartRPC(ctx context.Context, method string) (context.Context, *Span) {
	return ctx, &Span{}
}

func (t *Tracer) StartBackendCall(ctx context.Context, backend, operation string) (context.Context, *Span) {
	return ctx, &Span{}
}

func (t *Tracer) StartIngestion(ctx context.Context, repoID, phase string) (context.Context, *Span) {
	return ctx, &Span{}
}

func (t *Tracer) StartSearch(ctx context.Context, query string, backend string) (context.Context, *Span) {
	return ctx, &Span{}
}

func (t *Tracer) StartLLMCall(ctx context.Context, model string) (context.Context, *Span) {
	return ctx, &Span{}
}

// gRPC interceptors for automatic tracing

func (t *Tracer) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		return handler(ctx, req)
	}
}

func (t *Tracer) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		return handler(srv, stream)
	}
}

// Helper for extracting trace context from gRPC metadata
func (t *Tracer) extractTraceContext(ctx context.Context) context.Context {
	return ctx
}

// Span methods (no-op)

func (s *Span) End() {}

func (s *Span) RecordError(err error) {}

func (s *Span) SetStatus(code int, description string) {}

func (s *Span) SetAttributes(attrs ...Attribute) {}

// Helper functions

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// Span utilities

func SetSpanAttributes(span *Span, attrs ...Attribute) {
	// No-op
}

func RecordError(span *Span, err error, description string) {
	// No-op
}

func RecordSuccess(span *Span, description string) {
	// No-op
}

// Common attribute constructors

func RepositoryAttr(repoID string) Attribute {
	return Attribute{Key: "repository.id", Value: repoID}
}

func TenantAttr(tenantID string) Attribute {
	return Attribute{Key: "tenant.id", Value: tenantID}
}

func QueryAttr(query string) Attribute {
	return Attribute{Key: "search.query", Value: truncateString(query, 100)}
}

func BackendAttr(backend string) Attribute {
	return Attribute{Key: "backend.name", Value: backend}
}

func ResultCountAttr(count int) Attribute {
	return Attribute{Key: "search.result_count", Value: count}
}

func CacheHitAttr(hit bool) Attribute {
	return Attribute{Key: "cache.hit", Value: hit}
}

func FilePathAttr(path string) Attribute {
	return Attribute{Key: "file.path", Value: path}
}

func ModelAttr(model string) Attribute {
	return Attribute{Key: "model.name", Value: model}
}