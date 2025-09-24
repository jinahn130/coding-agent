package interceptors

import (
	"context"
	"strings"

	"repo-context-service/internal/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type AuthInterceptor struct {
	config *config.SecurityConfig
}

func NewAuthInterceptor(cfg *config.SecurityConfig) *AuthInterceptor {
	return &AuthInterceptor{
		config: cfg,
	}
}

func (a *AuthInterceptor) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Skip auth for health checks
		if isHealthCheckMethod(info.FullMethod) {
			return handler(ctx, req)
		}

		// Extract and validate auth
		newCtx, err := a.authenticate(ctx)
		if err != nil {
			return nil, err
		}

		return handler(newCtx, req)
	}
}

func (a *AuthInterceptor) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		// Skip auth for health checks
		if isHealthCheckMethod(info.FullMethod) {
			return handler(srv, stream)
		}

		// Extract and validate auth
		newCtx, err := a.authenticate(stream.Context())
		if err != nil {
			return err
		}

		// Wrap stream with authenticated context
		wrappedStream := &authenticatedStream{
			ServerStream: stream,
			ctx:         newCtx,
		}

		return handler(srv, wrappedStream)
	}
}

func (a *AuthInterceptor) authenticate(ctx context.Context) (context.Context, error) {
	if !a.config.RequireAuth {
		// Add default tenant to context
		return withTenantID(ctx, a.config.DefaultTenant), nil
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "missing metadata")
	}

	// Try API key first
	if apiKeys := md.Get("x-api-key"); len(apiKeys) > 0 {
		tenantID, err := a.validateAPIKey(apiKeys[0])
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid API key")
		}
		return withTenantID(ctx, tenantID), nil
	}

	// Try Authorization header
	if authHeaders := md.Get("authorization"); len(authHeaders) > 0 {
		tenantID, err := a.validateAuthHeader(authHeaders[0])
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid authorization")
		}
		return withTenantID(ctx, tenantID), nil
	}

	return nil, status.Errorf(codes.Unauthenticated, "missing authentication")
}

func (a *AuthInterceptor) validateAPIKey(apiKey string) (string, error) {
	// In a real implementation, you would validate the API key against a database
	// or key management service and extract the tenant ID

	// For now, use a simple validation
	if apiKey == "" {
		return "", status.Errorf(codes.Unauthenticated, "empty API key")
	}

	// Extract tenant from API key (simplified)
	// Format: tenant-apikey-randomstring
	parts := strings.Split(apiKey, "-")
	if len(parts) < 2 {
		return a.config.DefaultTenant, nil
	}

	return parts[0], nil
}

func (a *AuthInterceptor) validateAuthHeader(authHeader string) (string, error) {
	// Handle Bearer tokens
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		return a.validateBearerToken(token)
	}

	return "", status.Errorf(codes.Unauthenticated, "unsupported authorization type")
}

func (a *AuthInterceptor) validateBearerToken(token string) (string, error) {
	// In a real implementation, you would validate the JWT token
	// and extract the tenant ID from the claims

	// For now, use a simple validation
	if token == "" {
		return "", status.Errorf(codes.Unauthenticated, "empty token")
	}

	// For demo purposes, return default tenant
	return a.config.DefaultTenant, nil
}

// Context helpers

type tenantKey struct{}

func withTenantID(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenantID)
}

func GetTenantID(ctx context.Context) string {
	if tenantID, ok := ctx.Value(tenantKey{}).(string); ok {
		return tenantID
	}
	return "unknown"
}

// Stream wrapper

type authenticatedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *authenticatedStream) Context() context.Context {
	return s.ctx
}

// Helper functions

func isHealthCheckMethod(fullMethod string) bool {
	healthMethods := []string{
		"/repocontext.v1.HealthService/Check",
		"/repocontext.v1.HealthService/Ping",
		"/grpc.health.v1.Health/Check",
	}

	for _, method := range healthMethods {
		if fullMethod == method {
			return true
		}
	}

	return false
}