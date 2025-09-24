package composer

import (
	"context"
	"fmt"
	"time"

	"repo-context-service/internal/config"
	"repo-context-service/internal/observability"
	"github.com/sashabaranov/go-openai"
)

type OpenAIEmbeddingClient struct {
	client  *openai.Client
	config  config.OpenAIConfig
	metrics *observability.Metrics
	tracer  *observability.Tracer
}

func NewOpenAIEmbeddingClient(cfg config.OpenAIConfig, metrics *observability.Metrics, tracer *observability.Tracer) *OpenAIEmbeddingClient {
	client := openai.NewClient(cfg.APIKey)

	return &OpenAIEmbeddingClient{
		client:  client,
		config:  cfg,
		metrics: metrics,
		tracer:  tracer,
	}
}

func (c *OpenAIEmbeddingClient) GenerateEmbeddings(ctx context.Context, texts []string, model string) ([][]float32, error) {
	ctx, span := c.tracer.Start(ctx, "openai.embeddings")
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.ModelAttr(model),
		observability.ResultCountAttr(len(texts)),
	)

	if len(texts) == 0 {
		return nil, nil
	}

	// OpenAI has limits on batch size and token count
	// Process in batches to stay under limits
	batchSize := 100 // Conservative batch size
	var allEmbeddings [][]float32

	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}

		batch := texts[i:end]
		embeddings, err := c.generateEmbeddingsBatch(ctx, batch, model)
		if err != nil {
			return nil, fmt.Errorf("failed to generate embeddings for batch %d-%d: %w", i, end, err)
		}

		allEmbeddings = append(allEmbeddings, embeddings...)
	}

	return allEmbeddings, nil
}

func (c *OpenAIEmbeddingClient) generateEmbeddingsBatch(ctx context.Context, texts []string, model string) ([][]float32, error) {
	timer := observability.StartTimer()
	defer func() {
		c.metrics.RecordBackendLatency("openai", timer.Duration())
	}()

	// Validate model parameter
	if model == "" {
		return nil, fmt.Errorf("embedding model parameter is empty")
	}

	// Create request with model string (newer models not supported as constants in v1.15.3)
	var embeddingModel openai.EmbeddingModel
	switch model {
	case "text-embedding-ada-002":
		embeddingModel = openai.AdaEmbeddingV2
	default:
		// For newer models (text-embedding-3-*) that aren't supported as constants,
		// we need to use a different approach or fall back to ada-002
		embeddingModel = openai.AdaEmbeddingV2
		fmt.Printf("WARNING: Model %s not supported as constant, falling back to text-embedding-ada-002\n", model)
	}

	req := openai.EmbeddingRequestStrings{
		Input: texts,
		Model: embeddingModel,
	}

	// Set timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, c.config.Timeout)
	defer cancel()

	// Make API call
	resp, err := c.client.CreateEmbeddings(timeoutCtx, req)
	if err != nil {
		c.metrics.RecordEmbeddingRequest(model, "error")
		return nil, fmt.Errorf("OpenAI embeddings API call failed: %w", err)
	}

	c.metrics.RecordEmbeddingRequest(model, "success")

	// Extract embeddings
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("response embedding count mismatch: got %d, expected %d", len(resp.Data), len(texts))
	}

	embeddings := make([][]float32, len(resp.Data))
	for i, data := range resp.Data {
		embeddings[i] = data.Embedding
	}

	return embeddings, nil
}

func (c *OpenAIEmbeddingClient) GetEmbeddingDimensions(model string) int {
	// Return dimensions for known models
	switch model {
	case "text-embedding-3-small":
		return 1536
	case "text-embedding-3-large":
		return 3072
	case "text-embedding-ada-002":
		return 1536
	default:
		return 1536 // Default fallback
	}
}

func (c *OpenAIEmbeddingClient) ValidateModel(model string) error {
	validModels := map[string]bool{
		"text-embedding-3-small": true,
		"text-embedding-3-large": true,
		"text-embedding-ada-002": true,
	}

	if !validModels[model] {
		return fmt.Errorf("unsupported embedding model: %s", model)
	}

	return nil
}

func (c *OpenAIEmbeddingClient) GetDefaultModel() string {
	model := c.config.Model
	if model == "" {
		model = "text-embedding-3-small" // Fallback default
	}
	return model
}

// Helper function to estimate token count (rough approximation)
func estimateTokenCount(text string) int {
	// Rough estimation: 1 token ≈ 4 characters for English text
	return len(text) / 4
}

// Helper function to truncate text to fit within token limits
func truncateToTokenLimit(text string, maxTokens int) string {
	estimatedTokens := estimateTokenCount(text)
	if estimatedTokens <= maxTokens {
		return text
	}

	// Truncate to approximately fit within token limit
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}

	return text[:maxChars-3] + "..."
}

// Batch processing helper
type EmbeddingBatch struct {
	Texts   []string
	Indices []int // Original indices in the input slice
}

func createBatches(texts []string, batchSize int, maxTokensPerBatch int) []EmbeddingBatch {
	var batches []EmbeddingBatch
	var currentBatch EmbeddingBatch
	currentTokens := 0

	for i, text := range texts {
		textTokens := estimateTokenCount(text)

		// If adding this text would exceed limits, start a new batch
		if (len(currentBatch.Texts) >= batchSize) ||
		   (currentTokens + textTokens > maxTokensPerBatch && len(currentBatch.Texts) > 0) {
			batches = append(batches, currentBatch)
			currentBatch = EmbeddingBatch{}
			currentTokens = 0
		}

		// Truncate text if it's too long for a single batch
		if textTokens > maxTokensPerBatch {
			text = truncateToTokenLimit(text, maxTokensPerBatch)
			textTokens = estimateTokenCount(text)
		}

		currentBatch.Texts = append(currentBatch.Texts, text)
		currentBatch.Indices = append(currentBatch.Indices, i)
		currentTokens += textTokens
	}

	// Add the last batch if it has content
	if len(currentBatch.Texts) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

// Advanced embedding generation with retry logic
func (c *OpenAIEmbeddingClient) GenerateEmbeddingsWithRetry(ctx context.Context, texts []string, model string, maxRetries int) ([][]float32, error) {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		embeddings, err := c.GenerateEmbeddings(ctx, texts, model)
		if err == nil {
			return embeddings, nil
		}

		lastErr = err

		// Don't retry on the last attempt
		if attempt == maxRetries {
			break
		}

		// Exponential backoff
		backoffDuration := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoffDuration):
			// Continue to next attempt
		}
	}

	return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
}