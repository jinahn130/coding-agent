package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Server     ServerConfig
	Redis      RedisConfig
	Weaviate   WeaviateConfig
	OpenAI     OpenAIConfig
	DeepSeek   DeepSeekConfig
	Upload     UploadConfig
	Observability ObservabilityConfig
	Security   SecurityConfig
	Defaults   DefaultsConfig
}

type ServerConfig struct {
	HTTPPort     int
	GRPCPort     int
	AdminPort    int
	Environment  string
	LogLevel     string
	GracefulShutdownTimeout time.Duration
}

type RedisConfig struct {
	URL      string
	Password string
	DB       int
	PoolSize int
	TTL      TTLConfig
}

type TTLConfig struct {
	RepositoryRouting time.Duration
	QueryResults      time.Duration
	UploadStatus      time.Duration
}

type WeaviateConfig struct {
	URL    string
	APIKey string
	Scheme string
	Host   string
}

type OpenAIConfig struct {
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float32
	Timeout     time.Duration
}

type DeepSeekConfig struct {
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float32
	Timeout     time.Duration
	StreamTokens bool
}

type UploadConfig struct {
	MaxFileSize   int64
	MaxFiles      int
	TempDir       string
	StorageDir    string
	AllowedTypes  []string
	ExcludePatterns []string
}

type ObservabilityConfig struct {
	MetricsEnabled bool
	TracingEnabled bool
	PProfEnabled   bool
	TracingEndpoint string
	ServiceName    string
	ServiceVersion string
}

type SecurityConfig struct {
	RequireAuth    bool
	DefaultTenant  string
	RateLimit      RateLimitConfig
	CORS           CORSConfig
}

type RateLimitConfig struct {
	RequestsPerSecond int
	BurstSize         int
	WindowSize        time.Duration
}

type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
}

type DefaultsConfig struct {
	MaxSearchResults int
	SearchTimeout    time.Duration
	EmbeddingModel   string
	ChunkSize        int
	ChunkOverlap     int
}

func Load() (*Config, error) {
	config := &Config{
		Server: ServerConfig{
			HTTPPort:                getEnvInt("HTTP_PORT", 8080),
			GRPCPort:                getEnvInt("GRPC_PORT", 9090),
			AdminPort:               getEnvInt("ADMIN_PORT", 8081),
			Environment:             getEnvString("ENVIRONMENT", "development"),
			LogLevel:                getEnvString("LOG_LEVEL", "info"),
			GracefulShutdownTimeout: getEnvDuration("GRACEFUL_SHUTDOWN_TIMEOUT", 30*time.Second),
		},
		Redis: RedisConfig{
			URL:      getEnvString("REDIS_URL", "redis://localhost:6379"),
			Password: getEnvString("REDIS_PASSWORD", ""),
			DB:       getEnvInt("REDIS_DB", 0),
			PoolSize: getEnvInt("REDIS_POOL_SIZE", 10),
			TTL: TTLConfig{
				RepositoryRouting: getEnvDuration("REDIS_TTL_REPO_ROUTING", 24*time.Hour),
				QueryResults:      getEnvDuration("REDIS_TTL_QUERY_RESULTS", 5*time.Minute),
				UploadStatus:      getEnvDuration("REDIS_TTL_UPLOAD_STATUS", 15*time.Minute),
			},
		},
		Weaviate: WeaviateConfig{
			URL:    getEnvString("WEAVIATE_URL", "https://your-cluster.weaviate.network"),
			APIKey: getEnvString("WEAVIATE_API_KEY", ""),
			Scheme: getEnvString("WEAVIATE_SCHEME", "https"),
			Host:   getEnvString("WEAVIATE_HOST", "your-cluster.weaviate.network"),
		},
		OpenAI: OpenAIConfig{
			APIKey:      getEnvString("OPENAI_API_KEY", ""),
			Model:       getEnvString("OPENAI_MODEL", "text-embedding-3-small"),
			MaxTokens:   getEnvInt("OPENAI_MAX_TOKENS", 8191),
			Temperature: getEnvFloat32("OPENAI_TEMPERATURE", 0.0),
			Timeout:     getEnvDuration("OPENAI_TIMEOUT", 30*time.Second),
		},
		DeepSeek: DeepSeekConfig{
			APIKey:       getEnvString("DEEPSEEK_API_KEY", ""),
			Model:        getEnvString("DEEPSEEK_MODEL", "deepseek-chat"),
			MaxTokens:    getEnvInt("DEEPSEEK_MAX_TOKENS", 4096),
			Temperature:  getEnvFloat32("DEEPSEEK_TEMPERATURE", 0.1),
			Timeout:      getEnvDuration("DEEPSEEK_TIMEOUT", 60*time.Second),
			StreamTokens: getEnvBool("DEEPSEEK_STREAM_TOKENS", true),
		},
		Upload: UploadConfig{
			MaxFileSize:  getEnvInt64("UPLOAD_MAX_FILE_SIZE", 100*1024*1024), // 100MB
			MaxFiles:     getEnvInt("UPLOAD_MAX_FILES", 10000),
			TempDir:      getEnvString("UPLOAD_TEMP_DIR", "/tmp/repo-uploads"),
			StorageDir:   getEnvString("UPLOAD_STORAGE_DIR", "./data/repositories"),
			AllowedTypes: getEnvStringSlice("UPLOAD_ALLOWED_TYPES", []string{".zip", ".tar", ".tar.gz", ".tgz"}),
			ExcludePatterns: getEnvStringSlice("UPLOAD_EXCLUDE_PATTERNS", []string{
				"node_modules/", "vendor/", ".git/", "*.exe", "*.dll", "*.so", "*.dylib",
				"*.jpg", "*.png", "*.gif", "*.pdf", "*.mp4", "*.zip", "*.tar.gz",
			}),
		},
		Observability: ObservabilityConfig{
			MetricsEnabled:  getEnvBool("METRICS_ENABLED", true),
			TracingEnabled:  getEnvBool("TRACING_ENABLED", true),
			PProfEnabled:    getEnvBool("PPROF_ENABLED", true),
			TracingEndpoint: getEnvString("TRACING_ENDPOINT", "http://localhost:14268/api/traces"),
			ServiceName:     getEnvString("SERVICE_NAME", "repo-context-service"),
			ServiceVersion:  getEnvString("SERVICE_VERSION", "1.0.0"),
		},
		Security: SecurityConfig{
			RequireAuth:   getEnvBool("REQUIRE_AUTH", false),
			DefaultTenant: getEnvString("DEFAULT_TENANT", "local"),
			RateLimit: RateLimitConfig{
				RequestsPerSecond: getEnvInt("RATE_LIMIT_RPS", 100),
				BurstSize:         getEnvInt("RATE_LIMIT_BURST", 200),
				WindowSize:        getEnvDuration("RATE_LIMIT_WINDOW", time.Minute),
			},
			CORS: CORSConfig{
				AllowedOrigins: getEnvStringSlice("CORS_ALLOWED_ORIGINS", []string{"*"}),
				AllowedMethods: getEnvStringSlice("CORS_ALLOWED_METHODS", []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}),
				AllowedHeaders: getEnvStringSlice("CORS_ALLOWED_HEADERS", []string{"*"}),
			},
		},
		Defaults: DefaultsConfig{
			MaxSearchResults: getEnvInt("DEFAULT_MAX_SEARCH_RESULTS", 20),
			SearchTimeout:    getEnvDuration("DEFAULT_SEARCH_TIMEOUT", 5*time.Second),
			EmbeddingModel:   getEnvString("DEFAULT_EMBEDDING_MODEL", "text-embedding-3-small"),
			ChunkSize:        getEnvInt("DEFAULT_CHUNK_SIZE", 100),
			ChunkOverlap:     getEnvInt("DEFAULT_CHUNK_OVERLAP", 10),
		},
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return config, nil
}

func (c *Config) Validate() error {
	if c.OpenAI.APIKey == "" {
		return fmt.Errorf("OPENAI_API_KEY is required")
	}

	if c.DeepSeek.APIKey == "" {
		return fmt.Errorf("DEEPSEEK_API_KEY is required")
	}

	if c.Weaviate.URL == "" {
		return fmt.Errorf("WEAVIATE_URL is required")
	}

	if c.Server.HTTPPort == c.Server.GRPCPort {
		return fmt.Errorf("HTTP_PORT and GRPC_PORT cannot be the same")
	}

	if c.Upload.MaxFileSize <= 0 {
		return fmt.Errorf("UPLOAD_MAX_FILE_SIZE must be positive")
	}

	return nil
}

func (c *Config) IsDevelopment() bool {
	return c.Server.Environment == "development"
}

func (c *Config) IsProduction() bool {
	return c.Server.Environment == "production"
}

// Helper functions for environment variables
func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getEnvFloat32(key string, defaultValue float32) float32 {
	if value := os.Getenv(key); value != "" {
		if floatValue, err := strconv.ParseFloat(value, 32); err == nil {
			return float32(floatValue)
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

func getEnvStringSlice(key string, defaultValue []string) []string {
	if value := os.Getenv(key); value != "" {
		return strings.Split(value, ",")
	}
	return defaultValue
}