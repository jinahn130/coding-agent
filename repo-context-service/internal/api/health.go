package api

import (
	"context"

	"repo-context-service/internal/cache"
	"repo-context-service/internal/config"
	"repo-context-service/internal/observability"
	"repo-context-service/internal/query"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type HealthServer struct {
	repocontextv1.UnimplementedHealthServiceServer
	config         *config.Config
	cache          *cache.RedisCache
	lexicalClient  *query.RipgrepClient
	semanticClient *query.WeaviateClient
	metrics        *observability.Metrics
	tracer         *observability.Tracer
}

func NewHealthServer(
	cfg *config.Config,
	cache *cache.RedisCache,
	lexicalClient *query.RipgrepClient,
	semanticClient *query.WeaviateClient,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *HealthServer {
	return &HealthServer{
		config:         cfg,
		cache:          cache,
		lexicalClient:  lexicalClient,
		semanticClient: semanticClient,
		metrics:        metrics,
		tracer:         tracer,
	}
}

func (s *HealthServer) Check(ctx context.Context, req *emptypb.Empty) (*repocontextv1.HealthCheckResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "HealthCheck")
	defer span.End()

	response := &repocontextv1.HealthCheckResponse{
		Status:     repocontextv1.HealthCheckResponse_SERVING_STATUS_SERVING,
		Components: []*repocontextv1.ComponentHealth{},
	}

	// Check Redis
	redisHealth := s.checkRedis(ctx)
	response.Components = append(response.Components, redisHealth)

	// Check Weaviate
	weaviateHealth := s.checkWeaviate(ctx)
	response.Components = append(response.Components, weaviateHealth)

	// Check Ripgrep
	ripgrepHealth := s.checkRipgrep(ctx)
	response.Components = append(response.Components, ripgrepHealth)

	// Determine overall status
	allHealthy := true
	for _, component := range response.Components {
		if component.Status != repocontextv1.HealthCheckResponse_SERVING_STATUS_SERVING {
			allHealthy = false
			break
		}
	}

	if !allHealthy {
		response.Status = repocontextv1.HealthCheckResponse_SERVING_STATUS_NOT_SERVING
	}

	return response, nil
}

func (s *HealthServer) Ping(ctx context.Context, req *emptypb.Empty) (*repocontextv1.PingResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "Ping")
	defer span.End()

	return &repocontextv1.PingResponse{
		Message:   "pong",
		Timestamp: timestamppb.Now(),
	}, nil
}

func (s *HealthServer) checkRedis(ctx context.Context) *repocontextv1.ComponentHealth {
	health := &repocontextv1.ComponentHealth{
		Name:   "redis",
		Status: repocontextv1.HealthCheckResponse_SERVING_STATUS_SERVING,
	}

	if err := s.cache.HealthCheck(ctx); err != nil {
		health.Status = repocontextv1.HealthCheckResponse_SERVING_STATUS_NOT_SERVING
		health.Message = err.Error()
	} else {
		health.Message = "Redis is healthy"
	}

	return health
}

func (s *HealthServer) checkWeaviate(ctx context.Context) *repocontextv1.ComponentHealth {
	health := &repocontextv1.ComponentHealth{
		Name:   "weaviate",
		Status: repocontextv1.HealthCheckResponse_SERVING_STATUS_SERVING,
	}

	if err := s.semanticClient.HealthCheck(ctx); err != nil {
		health.Status = repocontextv1.HealthCheckResponse_SERVING_STATUS_NOT_SERVING
		health.Message = err.Error()
	} else {
		health.Message = "Weaviate is healthy"
	}

	return health
}

func (s *HealthServer) checkRipgrep(ctx context.Context) *repocontextv1.ComponentHealth {
	health := &repocontextv1.ComponentHealth{
		Name:   "ripgrep",
		Status: repocontextv1.HealthCheckResponse_SERVING_STATUS_SERVING,
	}

	if err := s.lexicalClient.HealthCheck(ctx); err != nil {
		health.Status = repocontextv1.HealthCheckResponse_SERVING_STATUS_NOT_SERVING
		health.Message = err.Error()
	} else {
		health.Message = "Ripgrep is healthy"
	}

	return health
}