package interceptors

import (
	"context"
	"sync"
	"time"

	"repo-context-service/internal/config"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RateLimitInterceptor struct {
	config   *config.RateLimitConfig
	limiters map[string]*rate.Limiter
	mutex    sync.RWMutex
}

func NewRateLimitInterceptor(cfg *config.RateLimitConfig) *RateLimitInterceptor {
	return &RateLimitInterceptor{
		config:   cfg,
		limiters: make(map[string]*rate.Limiter),
	}
}

func (r *RateLimitInterceptor) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Skip rate limiting for health checks
		if isHealthCheckMethod(info.FullMethod) {
			return handler(ctx, req)
		}

		if err := r.checkRateLimit(ctx); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

func (r *RateLimitInterceptor) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		// Skip rate limiting for health checks
		if isHealthCheckMethod(info.FullMethod) {
			return handler(srv, stream)
		}

		if err := r.checkRateLimit(stream.Context()); err != nil {
			return err
		}

		return handler(srv, stream)
	}
}

func (r *RateLimitInterceptor) checkRateLimit(ctx context.Context) error {
	tenantID := GetTenantID(ctx)
	if tenantID == "" {
		tenantID = "default"
	}

	limiter := r.getLimiter(tenantID)

	// Check if request is allowed
	if !limiter.Allow() {
		return status.Errorf(codes.ResourceExhausted, "rate limit exceeded for tenant %s", tenantID)
	}

	return nil
}

func (r *RateLimitInterceptor) getLimiter(tenantID string) *rate.Limiter {
	r.mutex.RLock()
	limiter, exists := r.limiters[tenantID]
	r.mutex.RUnlock()

	if exists {
		return limiter
	}

	// Create new limiter for tenant
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Double-check after acquiring write lock
	if limiter, exists := r.limiters[tenantID]; exists {
		return limiter
	}

	// Create rate limiter with per-second rate and burst size
	limiter = rate.NewLimiter(
		rate.Limit(r.config.RequestsPerSecond),
		r.config.BurstSize,
	)

	r.limiters[tenantID] = limiter

	// Start cleanup routine for this tenant (simplified)
	go r.cleanupLimiter(tenantID, r.config.WindowSize*2)

	return limiter
}

func (r *RateLimitInterceptor) cleanupLimiter(tenantID string, cleanupAfter time.Duration) {
	time.Sleep(cleanupAfter)

	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Remove inactive limiters to prevent memory leaks
	// In a production system, you'd want more sophisticated cleanup logic
	delete(r.limiters, tenantID)
}