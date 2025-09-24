package cache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type RedisCache struct {
	client *redis.Client
	ttl    TTLConfig
}

type TTLConfig struct {
	RepositoryRouting time.Duration
	QueryResults      time.Duration
	UploadStatus      time.Duration
}

type CachedUploadStatus struct {
	UploadID     string                         `json:"upload_id"`
	RepositoryID string                         `json:"repository_id"`
	Status       *repocontextv1.IngestionStatus `json:"status"`
	Progress     *repocontextv1.IngestionProgress `json:"progress"`
	ErrorMessage string                         `json:"error_message,omitempty"`
	CreatedAt    time.Time                      `json:"created_at"`
}

type CachedQueryResult struct {
	Chunks    []*repocontextv1.CodeChunk        `json:"chunks"`
	Timings   *repocontextv1.SearchTimings      `json:"timings"`
	Stats     *repocontextv1.SearchStats        `json:"stats"`
	CachedAt  time.Time                         `json:"cached_at"`
}

// Simplified repository metadata for Redis storage (avoids protobuf oneof issues)
type CachedRepositoryMetadata struct {
	RepositoryID    string                         `json:"repository_id"`
	Name            string                         `json:"name"`
	Description     string                         `json:"description"`
	GitURL          string                         `json:"git_url,omitempty"`
	UploadedFile    string                         `json:"uploaded_file,omitempty"`
	Ref             string                         `json:"ref,omitempty"`
	CommitSha       string                         `json:"commit_sha,omitempty"`
	IngestionStatus *repocontextv1.IngestionStatus `json:"ingestion_status"`
	Stats           *repocontextv1.RepositoryStats `json:"stats"`
	CreatedAt       time.Time                      `json:"created_at"`
	UpdatedAt       time.Time                      `json:"updated_at"`
}

func NewRedisCache(redisURL string, password string, db int, ttl TTLConfig) (*RedisCache, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	if password != "" {
		opts.Password = password
	}
	if db != 0 {
		opts.DB = db
	}

	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisCache{
		client: client,
		ttl:    ttl,
	}, nil
}

func (r *RedisCache) Close() error {
	return r.client.Close()
}

// Repository routing cache
func (r *RedisCache) SetRepositoryIndex(ctx context.Context, tenantID, repoKey, indexID string) error {
	key := r.repositoryKey(tenantID, repoKey)
	return r.client.Set(ctx, key, indexID, r.ttl.RepositoryRouting).Err()
}

func (r *RedisCache) GetRepositoryIndex(ctx context.Context, tenantID, repoKey string) (string, error) {
	key := r.repositoryKey(tenantID, repoKey)
	result, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return result, err
}

func (r *RedisCache) DeleteRepositoryIndex(ctx context.Context, tenantID, repoKey string) error {
	key := r.repositoryKey(tenantID, repoKey)
	return r.client.Del(ctx, key).Err()
}

// Upload status cache
func (r *RedisCache) SetUploadStatus(ctx context.Context, tenantID string, status *CachedUploadStatus) error {
	key := r.uploadStatusKey(tenantID, status.UploadID)
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal upload status: %w", err)
	}
	return r.client.Set(ctx, key, data, r.ttl.UploadStatus).Err()
}

func (r *RedisCache) GetUploadStatus(ctx context.Context, tenantID, uploadID string) (*CachedUploadStatus, error) {
	key := r.uploadStatusKey(tenantID, uploadID)
	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var status CachedUploadStatus
	if err := json.Unmarshal([]byte(data), &status); err != nil {
		return nil, fmt.Errorf("failed to unmarshal upload status: %w", err)
	}
	return &status, nil
}

func (r *RedisCache) DeleteUploadStatus(ctx context.Context, tenantID, uploadID string) error {
	key := r.uploadStatusKey(tenantID, uploadID)
	return r.client.Del(ctx, key).Err()
}

// Query results cache
func (r *RedisCache) SetQueryResult(ctx context.Context, tenantID, repoID, query string, topK int, result *CachedQueryResult) error {
	key := r.queryResultKey(tenantID, repoID, query, topK)
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("failed to marshal query result: %w", err)
	}
	return r.client.Set(ctx, key, data, r.ttl.QueryResults).Err()
}

func (r *RedisCache) GetQueryResult(ctx context.Context, tenantID, repoID, query string, topK int) (*CachedQueryResult, error) {
	key := r.queryResultKey(tenantID, repoID, query, topK)
	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var result CachedQueryResult
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal query result: %w", err)
	}
	return &result, nil
}

func (r *RedisCache) DeleteQueryResult(ctx context.Context, tenantID, repoID, query string, topK int) error {
	key := r.queryResultKey(tenantID, repoID, query, topK)
	return r.client.Del(ctx, key).Err()
}

// Repository metadata cache
func (r *RedisCache) SetRepositoryMetadata(ctx context.Context, tenantID string, repo *repocontextv1.Repository) error {
	key := r.repositoryMetadataKey(tenantID, repo.RepositoryId)

	// Convert to cacheable format
	cached := r.toCachedRepo(repo)
	data, err := json.Marshal(cached)
	if err != nil {
		return fmt.Errorf("failed to marshal repository metadata: %w", err)
	}

	return r.client.Set(ctx, key, data, r.ttl.RepositoryRouting).Err()
}

func (r *RedisCache) GetRepositoryMetadata(ctx context.Context, tenantID, repoID string) (*repocontextv1.Repository, error) {
	key := r.repositoryMetadataKey(tenantID, repoID)
	data, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var cached CachedRepositoryMetadata
	if err := json.Unmarshal([]byte(data), &cached); err != nil {
		return nil, fmt.Errorf("failed to unmarshal repository metadata: %w", err)
	}

	// Convert back to protobuf format
	return r.fromCachedRepo(&cached), nil
}

func (r *RedisCache) ListRepositoryMetadata(ctx context.Context, tenantID string) ([]*repocontextv1.Repository, error) {
	// Build pattern manually to avoid sanitizing the wildcard
	pattern := fmt.Sprintf("repo_meta:%s:*", sanitizeTenantID(tenantID))
	keys, err := r.client.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		return nil, nil
	}

	values, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}

	repositories := make([]*repocontextv1.Repository, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}

		var cached CachedRepositoryMetadata
		if err := json.Unmarshal([]byte(value.(string)), &cached); err != nil {
			continue // Skip invalid entries
		}

		repo := r.fromCachedRepo(&cached)
		repositories = append(repositories, repo)
	}

	return repositories, nil
}

func (r *RedisCache) DeleteRepositoryMetadata(ctx context.Context, tenantID, repoID string) error {
	key := r.repositoryMetadataKey(tenantID, repoID)
	return r.client.Del(ctx, key).Err()
}

// Key generation helpers
func (r *RedisCache) repositoryKey(tenantID, repoKey string) string {
	return fmt.Sprintf("repo_idx:%s:%s", sanitizeTenantID(tenantID), sanitizeRepoKey(repoKey))
}

func (r *RedisCache) uploadStatusKey(tenantID, uploadID string) string {
	return fmt.Sprintf("upload_status:%s:%s", sanitizeTenantID(tenantID), sanitizeID(uploadID))
}

func (r *RedisCache) queryResultKey(tenantID, repoID, query string, topK int) string {
	normalizedQuery := normalizeQuery(query)
	queryHash := hashString(normalizedQuery)
	return fmt.Sprintf("ctx_res:%s:%s|%s|k:%d",
		sanitizeTenantID(tenantID),
		sanitizeID(repoID),
		queryHash,
		topK)
}

func (r *RedisCache) repositoryMetadataKey(tenantID, repoID string) string {
	return fmt.Sprintf("repo_meta:%s:%s", sanitizeTenantID(tenantID), sanitizeID(repoID))
}

// Health check
func (r *RedisCache) HealthCheck(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// Utility functions
func sanitizeTenantID(tenantID string) string {
	// Allow alphanumeric and hyphens only
	reg := regexp.MustCompile(`[^a-zA-Z0-9\-]`)
	return reg.ReplaceAllString(tenantID, "_")
}

func sanitizeID(id string) string {
	// Allow alphanumeric, hyphens, and underscores only
	reg := regexp.MustCompile(`[^a-zA-Z0-9\-_]`)
	return reg.ReplaceAllString(id, "_")
}

func sanitizeRepoKey(repoKey string) string {
	// Allow alphanumeric, hyphens, underscores, slashes, and @ for git URLs
	reg := regexp.MustCompile(`[^a-zA-Z0-9\-_/@\.]`)
	return reg.ReplaceAllString(repoKey, "_")
}

func normalizeQuery(query string) string {
	// Normalize query for caching: lowercase, trim, collapse whitespace
	query = strings.ToLower(query)
	query = strings.TrimSpace(query)

	// Collapse multiple whitespace into single space
	reg := regexp.MustCompile(`\s+`)
	query = reg.ReplaceAllString(query, " ")

	// Remove punctuation except common programming symbols
	reg = regexp.MustCompile(`[^\w\s\.\-_\(\)\[\]\{\}]`)
	query = reg.ReplaceAllString(query, "")

	return query
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)[:16] // Use first 16 chars of hash
}

// Helper functions for converting between protobuf and cached formats

func (r *RedisCache) toCachedRepo(repo *repocontextv1.Repository) *CachedRepositoryMetadata {
	cached := &CachedRepositoryMetadata{
		RepositoryID:    repo.RepositoryId,
		Name:            repo.Name,
		Description:     repo.Description,
		IngestionStatus: repo.IngestionStatus,
		Stats:           repo.Stats,
		CreatedAt:       repo.CreatedAt.AsTime(),
		UpdatedAt:       repo.UpdatedAt.AsTime(),
	}

	if repo.Source != nil {
		cached.Ref = repo.Source.Ref
		cached.CommitSha = repo.Source.CommitSha

		switch source := repo.Source.Source.(type) {
		case *repocontextv1.RepositorySource_GitUrl:
			cached.GitURL = source.GitUrl
		case *repocontextv1.RepositorySource_UploadedFilename:
			cached.UploadedFile = source.UploadedFilename
		}
	}

	return cached
}

func (r *RedisCache) fromCachedRepo(cached *CachedRepositoryMetadata) *repocontextv1.Repository {
	repo := &repocontextv1.Repository{
		RepositoryId:    cached.RepositoryID,
		Name:            cached.Name,
		Description:     cached.Description,
		IngestionStatus: cached.IngestionStatus,
		Stats:           cached.Stats,
		CreatedAt:       timestamppb.New(cached.CreatedAt),
		UpdatedAt:       timestamppb.New(cached.UpdatedAt),
	}

	// Reconstruct source
	if cached.GitURL != "" || cached.UploadedFile != "" {
		repo.Source = &repocontextv1.RepositorySource{
			Ref:       cached.Ref,
			CommitSha: cached.CommitSha,
		}

		if cached.GitURL != "" {
			repo.Source.Source = &repocontextv1.RepositorySource_GitUrl{
				GitUrl: cached.GitURL,
			}
		} else if cached.UploadedFile != "" {
			repo.Source.Source = &repocontextv1.RepositorySource_UploadedFilename{
				UploadedFilename: cached.UploadedFile,
			}
		}
	}

	return repo
}