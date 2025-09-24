package query

import (
	"context"
	"fmt"
	"strings"

	"repo-context-service/internal/config"
	"repo-context-service/internal/ingest"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"github.com/weaviate/weaviate-go-client/v4/weaviate"
	"github.com/weaviate/weaviate-go-client/v4/weaviate/auth"
	"github.com/weaviate/weaviate-go-client/v4/weaviate/filters"
	"github.com/weaviate/weaviate-go-client/v4/weaviate/graphql"
	"github.com/weaviate/weaviate/entities/models"
)

type WeaviateClient struct {
	client  *weaviate.Client
	config  config.WeaviateConfig
	metrics *observability.Metrics
	tracer  *observability.Tracer
}

func NewWeaviateClient(cfg config.WeaviateConfig, metrics *observability.Metrics, tracer *observability.Tracer) (*WeaviateClient, error) {
	var authConfig auth.Config
	if cfg.APIKey != "" {
		authConfig = &auth.ApiKey{Value: cfg.APIKey}
	}

	config := weaviate.Config{
		Host:       cfg.Host,
		Scheme:     cfg.Scheme,
		AuthConfig: authConfig,
	}

	client, err := weaviate.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Weaviate client: %w", err)
	}

	return &WeaviateClient{
		client:  client,
		config:  cfg,
		metrics: metrics,
		tracer:  tracer,
	}, nil
}

func (w *WeaviateClient) CreateCollection(ctx context.Context, name string, dimensions int) error {
	ctx, span := w.tracer.StartBackendCall(ctx, "weaviate", "create_collection")
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.BackendAttr("weaviate"),
	)

	// Check if class already exists
	exists, err := w.client.Schema().ClassExistenceChecker().WithClassName(name).Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to check class existence: %w", err)
	}

	if exists {
		return nil // Class already exists
	}

	// Create class schema
	classObj := &models.Class{
		Class:       name,
		Description: fmt.Sprintf("Code chunks for repository %s", name),
		Vectorizer:  "none", // We provide our own vectors
		Properties: []*models.Property{
			{
				Name:        "repository_id",
				DataType:    []string{"string"},
				Description: "Repository identifier",
			},
			{
				Name:        "file_path",
				DataType:    []string{"string"},
				Description: "Path to the file within the repository",
			},
			{
				Name:        "start_line",
				DataType:    []string{"int"},
				Description: "Starting line number of the chunk",
			},
			{
				Name:        "end_line",
				DataType:    []string{"int"},
				Description: "Ending line number of the chunk",
			},
			{
				Name:        "content",
				DataType:    []string{"text"},
				Description: "Code content of the chunk",
			},
			{
				Name:        "language",
				DataType:    []string{"string"},
				Description: "Programming language of the code",
			},
			{
				Name:        "size",
				DataType:    []string{"int"},
				Description: "Size of the chunk in bytes",
			},
			{
				Name:        "created_at",
				DataType:    []string{"date"},
				Description: "When the chunk was created",
			},
		},
		VectorIndexConfig: map[string]interface{}{
			"distance": "cosine",
		},
	}

	timer := observability.StartTimer()
	err = w.client.Schema().ClassCreator().WithClass(classObj).Do(ctx)
	w.metrics.RecordBackendLatency("weaviate", timer.Duration())

	if err != nil {
		return fmt.Errorf("failed to create class: %w", err)
	}

	return nil
}

func (w *WeaviateClient) UpsertVectors(ctx context.Context, collectionName string, vectors []*ingest.Vector) error {
	ctx, span := w.tracer.StartBackendCall(ctx, "weaviate", "upsert_vectors")
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.BackendAttr("weaviate"),
		observability.ResultCountAttr(len(vectors)),
	)

	if len(vectors) == 0 {
		return nil
	}

	timer := observability.StartTimer()
	defer func() {
		w.metrics.RecordBackendLatency("weaviate", timer.Duration())
	}()

	// Convert to Weaviate objects
	objects := make([]*models.Object, len(vectors))
	for i, vector := range vectors {
		// Convert metadata to properties
		properties := make(map[string]interface{})
		for key, value := range vector.Metadata {
			properties[key] = value
		}

		strVector := make(models.Vector, len(vector.Vector))
		for j, v := range vector.Vector {
			strVector[j] = v
		}

		objects[i] = &models.Object{
			Class:      collectionName,
			Properties: properties,
		}
	}

	// Batch insert
	batcher := w.client.Batch().ObjectsBatcher()
	for _, obj := range objects {
		batcher.WithObject(obj)
	}

	_, err := batcher.Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to batch insert objects: %w", err)
	}

	return nil
}

func (w *WeaviateClient) DeleteCollection(ctx context.Context, name string) error {
	ctx, span := w.tracer.StartBackendCall(ctx, "weaviate", "delete_collection")
	defer span.End()

	timer := observability.StartTimer()
	err := w.client.Schema().ClassDeleter().WithClassName(name).Do(ctx)
	w.metrics.RecordBackendLatency("weaviate", timer.Duration())

	if err != nil {
		return fmt.Errorf("failed to delete class: %w", err)
	}

	return nil
}

func (w *WeaviateClient) SearchSemantic(ctx context.Context, repoID string, queryVector []float32, limit int, filters map[string]interface{}) ([]*repocontextv1.CodeChunk, error) {
	ctx, span := w.tracer.StartSearch(ctx, "", "semantic")
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.BackendAttr("weaviate"),
		observability.RepositoryAttr(repoID),
	)

	timer := observability.StartTimer()
	defer func() {
		w.metrics.RecordBackendLatency("weaviate", timer.Duration())
	}()

	// Build GraphQL query
	fields := []graphql.Field{
		{Name: "repository_id"},
		{Name: "file_path"},
		{Name: "start_line"},
		{Name: "end_line"},
		{Name: "content"},
		{Name: "language"},
		{Name: "size"},
		{Name: "_additional", Fields: []graphql.Field{
			{Name: "certainty"},
			{Name: "id"},
			{Name: "vector"},
		}},
	}

	nearVector := w.client.GraphQL().NearVectorArgBuilder().
		WithVector(queryVector).
		WithCertainty(0.7)

	query := w.client.GraphQL().Get().
		WithClassName(toWeaviateClassName(repoID)).
		WithFields(fields...).
		WithNearVector(nearVector).
		WithLimit(limit)

	// Add filters if specified
	if len(filters) > 0 {
		whereFilter := buildWhereFilter(filters)
		if whereFilter != nil {
			query = query.WithWhere(whereFilter)
		}
	}

	result, err := query.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search query: %w", err)
	}

	// Parse results
	chunks, err := w.parseSearchResults(result, repoID)
	if err != nil {
		return nil, fmt.Errorf("failed to parse search results: %w", err)
	}

	w.metrics.RecordSearchResults("semantic", len(chunks))

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(chunks)),
	)

	return chunks, nil
}

func (w *WeaviateClient) parseSearchResults(result *models.GraphQLResponse, repoID string) ([]*repocontextv1.CodeChunk, error) {
	if result.Errors != nil && len(result.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL errors: %v", result.Errors)
	}

	data, ok := result.Data["Get"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid response structure: missing Get")
	}

	classData, ok := data[repoID].([]interface{})
	if !ok {
		return nil, nil // No results found
	}

	var chunks []*repocontextv1.CodeChunk
	for _, item := range classData {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		chunk, err := w.parseChunkFromResult(itemMap, repoID)
		if err != nil {
			continue // Skip invalid chunks
		}

		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

func (w *WeaviateClient) parseChunkFromResult(data map[string]interface{}, repoID string) (*repocontextv1.CodeChunk, error) {
	chunk := &repocontextv1.CodeChunk{
		RepositoryId: repoID,
		Source:       repocontextv1.SearchSource_SEARCH_SOURCE_SEMANTIC,
	}

	// Extract properties
	if filePath, ok := data["file_path"].(string); ok {
		chunk.FilePath = filePath
	}

	if content, ok := data["content"].(string); ok {
		chunk.Content = content
	}

	if language, ok := data["language"].(string); ok {
		chunk.Language = language
	}

	if startLine, ok := data["start_line"].(float64); ok {
		chunk.StartLine = int32(startLine)
	}

	if endLine, ok := data["end_line"].(float64); ok {
		chunk.EndLine = int32(endLine)
	}

	// Extract score from _additional
	if additional, ok := data["_additional"].(map[string]interface{}); ok {
		if certainty, ok := additional["certainty"].(float64); ok {
			chunk.Score = float32(certainty)
		}
	}

	return chunk, nil
}

func buildWhereFilter(filterMap map[string]interface{}) *filters.WhereBuilder {
	var whereBuilder *filters.WhereBuilder

	// Add repository filter
	if repoID, ok := filterMap["repository_id"].(string); ok {
		condition := filters.Where().
			WithPath([]string{"repository_id"}).
			WithOperator(filters.Equal).
			WithValueText(repoID)

		if whereBuilder == nil {
			whereBuilder = condition
		} else {
			whereBuilder = whereBuilder.WithOperator(filters.And).WithOperands([]*filters.WhereBuilder{whereBuilder, condition})
		}
	}

	// Add language filter
	if language, ok := filterMap["language"].(string); ok {
		condition := filters.Where().
			WithPath([]string{"language"}).
			WithOperator(filters.Equal).
			WithValueText(language)

		if whereBuilder == nil {
			whereBuilder = condition
		} else {
			whereBuilder = whereBuilder.WithOperator(filters.And).WithOperands([]*filters.WhereBuilder{whereBuilder, condition})
		}
	}

	// Add file path prefix filter
	if pathPrefix, ok := filterMap["path_prefix"].(string); ok {
		condition := filters.Where().
			WithPath([]string{"file_path"}).
			WithOperator(filters.Like).
			WithValueText(pathPrefix)

		if whereBuilder == nil {
			whereBuilder = condition
		} else {
			whereBuilder = whereBuilder.WithOperator(filters.And).WithOperands([]*filters.WhereBuilder{whereBuilder, condition})
		}
	}

	return whereBuilder
}

func (w *WeaviateClient) HealthCheck(ctx context.Context) error {
	ctx, span := w.tracer.StartBackendCall(ctx, "weaviate", "health_check")
	defer span.End()

	timer := observability.StartTimer()
	defer func() {
		w.metrics.RecordBackendLatency("weaviate", timer.Duration())
	}()

	// Simple check by listing classes
	_, err := w.client.Schema().Getter().Do(ctx)
	return err
}

// Helper functions

func float32ToPointer(f float32) *float32 {
	return &f
}

func stringToPointer(s string) *string {
	return &s
}

func intToPointer(i int) *int {
	return &i
}

// toWeaviateClassName converts a repository ID to a valid Weaviate class name
// Weaviate class names must be PascalCase and contain no hyphens or special characters
func toWeaviateClassName(repoID string) string {
	return "Repo" + strings.ReplaceAll(strings.TrimPrefix(repoID, "repo-"), "-", "")
}