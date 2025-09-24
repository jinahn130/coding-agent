package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/gorilla/mux"
	"repo-context-service/internal/api"
	"repo-context-service/internal/cache"
	"repo-context-service/internal/composer"
	"repo-context-service/internal/config"
	"repo-context-service/internal/ingest"
	"repo-context-service/internal/interceptors"
	"repo-context-service/internal/observability"
	"repo-context-service/internal/query"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	_ "net/http/pprof"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Set up observability
	metrics := observability.NewMetrics()

	var tracer *observability.Tracer
	var tracerCleanup func()

	if cfg.Observability.TracingEnabled {
		var err error
		tracer, tracerCleanup, err = observability.NewTracer(
			cfg.Observability.ServiceName,
			cfg.Observability.ServiceVersion,
			cfg.Observability.TracingEndpoint,
		)
		if err != nil {
			log.Fatalf("Failed to create tracer: %v", err)
		}
		defer tracerCleanup()
	} else {
		// Create a no-op tracer
		tracer = observability.NewNoOpTracer()
		tracerCleanup = func() {}
	}

	// Set up Redis cache
	redisCache, err := cache.NewRedisCache(
		cfg.Redis.URL,
		cfg.Redis.Password,
		cfg.Redis.DB,
		cache.TTLConfig{
			RepositoryRouting: cfg.Redis.TTL.RepositoryRouting,
			QueryResults:      cfg.Redis.TTL.QueryResults,
			UploadStatus:      cfg.Redis.TTL.UploadStatus,
		},
	)
	if err != nil {
		log.Fatalf("Failed to create Redis cache: %v", err)
	}
	defer redisCache.Close()

	// Set up OpenAI embedding client
	embeddingClient := composer.NewOpenAIEmbeddingClient(cfg.OpenAI, metrics, tracer)

	// Set up Weaviate client
	weaviateClient, err := query.NewWeaviateClient(cfg.Weaviate, metrics, tracer)
	if err != nil {
		log.Fatalf("Failed to create Weaviate client: %v", err)
	}

	// Set up Ripgrep client
	ripgrepClient := query.NewRipgrepClient(metrics, tracer, cfg.Upload.StorageDir)

	// Set up result merger
	resultMerger := query.NewResultMerger(cfg.Defaults.MaxSearchResults)

	// Set up DeepSeek client
	deepSeekClient := composer.NewDeepSeekClient(cfg.DeepSeek, metrics, tracer)

	// Set up ingestion provider
	ingestProvider := ingest.NewInlineProcessor(
		redisCache,
		metrics,
		tracer,
		embeddingClient,
		weaviateClient,
		cfg.Upload.StorageDir,
		cfg.Upload.TempDir,
	)

	// Set up query service
	queryService := api.NewQueryService(
		ripgrepClient,
		weaviateClient,
		resultMerger,
		redisCache,
		metrics,
		tracer,
	)

	// Create gRPC server
	grpcServer := createGRPCServer(cfg, redisCache, ingestProvider, queryService, deepSeekClient, embeddingClient, metrics, tracer)

	// Create HTTP gateway server
	httpServer := createHTTPServer(cfg, grpcServer, redisCache, queryService, deepSeekClient, embeddingClient, metrics, tracer)

	// Start admin server (metrics, pprof)
	adminServer := createAdminServer(cfg, metrics)

	// Start servers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start gRPC server
	go func() {
		log.Printf("Starting gRPC server on port %d", cfg.Server.GRPCPort)
		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.GRPCPort))
		if err != nil {
			log.Fatalf("Failed to listen on gRPC port: %v", err)
		}

		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC server failed: %v", err)
		}
	}()

	// Start HTTP server
	go func() {
		log.Printf("Starting HTTP server on port %d", cfg.Server.HTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// Start admin server
	go func() {
		log.Printf("Starting admin server on port %d", cfg.Server.AdminPort)
		if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Admin server failed: %v", err)
		}
	}()

	// Wait for shutdown signal
	waitForShutdown(ctx, cancel, cfg.Server.GracefulShutdownTimeout, grpcServer, httpServer, adminServer)
}

func createGRPCServer(
	cfg *config.Config,
	cache *cache.RedisCache,
	ingestProvider ingest.Provider,
	queryService *api.QueryService,
	deepSeekClient *composer.DeepSeekClient,
	embeddingClient *composer.OpenAIEmbeddingClient,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *grpc.Server {
	// Create interceptors
	authInterceptor := interceptors.NewAuthInterceptor(&cfg.Security)
	rateLimitInterceptor := interceptors.NewRateLimitInterceptor(&cfg.Security.RateLimit)

	// Set up interceptor chain
	unaryInterceptors := []grpc.UnaryServerInterceptor{
		authInterceptor.UnaryServerInterceptor(),
		rateLimitInterceptor.UnaryServerInterceptor(),
		tracer.UnaryServerInterceptor(),
		metrics.UnaryServerInterceptor(),
	}

	streamInterceptors := []grpc.StreamServerInterceptor{
		authInterceptor.StreamServerInterceptor(),
		rateLimitInterceptor.StreamServerInterceptor(),
		tracer.StreamServerInterceptor(),
		metrics.StreamServerInterceptor(),
	}

	// Create gRPC server with interceptors
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(streamInterceptors...),
	)

	// Register services
	uploadServer := api.NewUploadServer(cfg, cache, ingestProvider, metrics, tracer)
	repocontextv1.RegisterUploadServiceServer(server, uploadServer)

	repositoryServer := api.NewRepositoryServer(cfg, cache, ingestProvider, metrics, tracer)
	repocontextv1.RegisterRepositoryServiceServer(server, repositoryServer)

	chatServer := api.NewChatServer(cfg, cache, queryService, deepSeekClient, embeddingClient, metrics, tracer)
	repocontextv1.RegisterChatServiceServer(server, chatServer)

	healthServer := api.NewHealthServer(cfg, cache, queryService.GetLexicalClient(), queryService.GetSemanticClient(), metrics, tracer)
	repocontextv1.RegisterHealthServiceServer(server, healthServer)

	// Enable reflection for grpcurl
	reflection.Register(server)

	return server
}

func createHTTPServer(
	cfg *config.Config,
	grpcServer *grpc.Server,
	cache *cache.RedisCache,
	queryService *api.QueryService,
	deepSeekClient *composer.DeepSeekClient,
	embeddingClient *composer.OpenAIEmbeddingClient,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *http.Server {
	// Create Gorilla Mux router for WebSocket and other routes
	router := mux.NewRouter()

	// Create gRPC-Gateway mux
	gwMux := runtime.NewServeMux()

	// Register gRPC-Gateway
	ctx := context.Background()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	grpcEndpoint := fmt.Sprintf("localhost:%d", cfg.Server.GRPCPort)

	if err := repocontextv1.RegisterUploadServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, opts); err != nil {
		log.Fatalf("Failed to register upload service handler: %v", err)
	}

	if err := repocontextv1.RegisterRepositoryServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, opts); err != nil {
		log.Fatalf("Failed to register repository service handler: %v", err)
	}

	// Skip ChatService registration - it's gRPC-only (bidirectional streaming)
	// WebSocket chat is handled separately via our custom WebSocket bridge
	// if err := repocontextv1.RegisterChatServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, opts); err != nil {
	// 	log.Fatalf("Failed to register chat service handler: %v", err)
	// }

	if err := repocontextv1.RegisterHealthServiceHandlerFromEndpoint(ctx, gwMux, grpcEndpoint, opts); err != nil {
		log.Fatalf("Failed to register health service handler: %v", err)
	}

	// Create ChatServer for WebSocket handler
	chatServer := api.NewChatServer(cfg, cache, queryService, deepSeekClient, embeddingClient, metrics, tracer)

	// Create WebSocket handler and register BEFORE gRPC-Gateway
	wsHandler := api.NewChatWebSocketHandler(chatServer, cfg, metrics, tracer)
	wsHandler.RegisterRoutes(router)

	// Mount gRPC-Gateway AFTER WebSocket routes to avoid conflicts
	router.PathPrefix("/").Handler(corsMiddleware(gwMux, &cfg.Security.CORS))

	// Create HTTP server
	return &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.HTTPPort),
		Handler:      router,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
}

func createAdminServer(cfg *config.Config, metrics *observability.Metrics) *http.Server {
	mux := http.NewServeMux()

	// Metrics endpoint
	if cfg.Observability.MetricsEnabled {
		mux.Handle("/metrics", metrics.Handler())
	}

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// pprof endpoints (only accessible from localhost)
	if cfg.Observability.PProfEnabled && cfg.IsDevelopment() {
		mux.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)
	}

	return &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", cfg.Server.AdminPort),
		Handler: mux,
	}
}

func corsMiddleware(handler http.Handler, corsConfig *config.CORSConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		if len(corsConfig.AllowedOrigins) > 0 {
			origin := r.Header.Get("Origin")
			for _, allowedOrigin := range corsConfig.AllowedOrigins {
				if allowedOrigin == "*" || allowedOrigin == origin {
					w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
					break
				}
			}
		}

		if len(corsConfig.AllowedMethods) > 0 {
			w.Header().Set("Access-Control-Allow-Methods", joinStrings(corsConfig.AllowedMethods, ", "))
		}

		if len(corsConfig.AllowedHeaders) > 0 {
			w.Header().Set("Access-Control-Allow-Headers", joinStrings(corsConfig.AllowedHeaders, ", "))
		}

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		handler.ServeHTTP(w, r)
	})
}

func waitForShutdown(ctx context.Context, cancel context.CancelFunc, timeout time.Duration, grpcServer *grpc.Server, httpServer *http.Server, adminServer *http.Server) {
	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal %v, starting graceful shutdown...", sig)

	// Cancel context to signal all goroutines to stop
	cancel()

	// Create shutdown context with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), timeout)
	defer shutdownCancel()

	// Shutdown servers gracefully
	var shutdownComplete = make(chan struct{})

	go func() {
		defer close(shutdownComplete)

		// Stop gRPC server
		log.Println("Stopping gRPC server...")
		grpcServer.GracefulStop()

		// Stop HTTP server
		log.Println("Stopping HTTP server...")
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}

		// Stop admin server
		log.Println("Stopping admin server...")
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Admin server shutdown error: %v", err)
		}

		log.Println("All servers stopped")
	}()

	// Wait for shutdown to complete or timeout
	select {
	case <-shutdownComplete:
		log.Println("Graceful shutdown completed")
	case <-shutdownCtx.Done():
		log.Println("Shutdown timeout exceeded, forcing exit")
	}
}

// Helper function to join strings
func joinStrings(slice []string, separator string) string {
	if len(slice) == 0 {
		return ""
	}
	result := slice[0]
	for i := 1; i < len(slice); i++ {
		result += separator + slice[i]
	}
	return result
}

