# Semantic Search Deep Dive

This document provides comprehensive technical details about the semantic search system, including vector embeddings, similarity matching, query processing, and performance optimization strategies.

## Overview: Vector-Based Code Understanding

The semantic search system transforms code into high-dimensional vectors that capture semantic meaning, enabling conceptual similarity matching beyond exact text matching.

```
Code Text → Embeddings → Vector Storage → Similarity Search → Ranked Results
   │              │            │               │                    │
   │              │            │               │                    │
 "function"   [0.123,        Weaviate      Query Vector      Cosine Similarity
 "auth"      -0.456,       Collection     [0.234, -0.567,     Score: 0.89
 "handler"    0.789,        Indexed         0.123, ...]        (High Match)
              ...]         Vectors
```

## Core Components Architecture

### 1. OpenAI Embedding Generation
**Location**: `internal/composer/openai_embedding.go`

```go
// GenerateEmbeddings creates vector representations of code chunks
// Parameters:
//   - texts: Array of code chunks (max 2048 tokens each)
//   - model: "text-embedding-ada-002" (1536 dimensions)
//   - batchSize: Optimal 50-100 texts per API call
// Returns:
//   - [][]float32: Array of 1536-dimensional vectors
//   - error: Rate limiting, quota, or API errors
func (c *OpenAIEmbeddingClient) GenerateEmbeddings(
    ctx context.Context,
    texts []string,
    model string,
) ([][]float32, error) {
    // Input validation and preprocessing
    if len(texts) == 0 {
        return nil, fmt.Errorf("no texts provided for embedding")
    }
    if len(texts) > c.maxBatchSize {
        return nil, fmt.Errorf("batch size %d exceeds limit %d", len(texts), c.maxBatchSize)
    }

    // Preprocess texts to optimize embedding quality
    processedTexts := make([]string, len(texts))
    for i, text := range texts {
        processedTexts[i] = c.preprocessCodeForEmbedding(text)
    }

    // Create OpenAI API request
    req := &openai.EmbeddingRequest{
        Input:      processedTexts,
        Model:      model,
        User:       "repo-context-service",  // For usage tracking
        Dimensions: 1536,  // text-embedding-ada-002 native dimension
    }

    // Execute API call with retry and rate limiting
    resp, err := c.executeWithRetry(ctx, req)
    if err != nil {
        return nil, fmt.Errorf("OpenAI API call failed: %w", err)
    }

    // Extract and validate embeddings
    embeddings := make([][]float32, len(resp.Data))
    for i, embeddingData := range resp.Data {
        if len(embeddingData.Embedding) != 1536 {
            return nil, fmt.Errorf("unexpected embedding dimension: got %d, expected 1536",
                                 len(embeddingData.Embedding))
        }

        // Convert float64 to float32 for memory efficiency
        embeddings[i] = make([]float32, len(embeddingData.Embedding))
        for j, val := range embeddingData.Embedding {
            embeddings[i][j] = float32(val)
        }
    }

    // Record metrics for monitoring
    c.metrics.RecordEmbeddingGeneration(len(texts), len(embeddings))
    c.metrics.RecordAPILatency("openai_embeddings", time.Since(startTime))

    return embeddings, nil
}

// preprocessCodeForEmbedding optimizes code text for better embeddings
// - Removes excessive whitespace while preserving structure
// - Normalizes common code patterns
// - Adds context markers for better semantic understanding
func (c *OpenAIEmbeddingClient) preprocessCodeForEmbedding(code string) string {
    // Normalize whitespace but preserve indentation structure
    lines := strings.Split(code, "\n")
    var processedLines []string

    for _, line := range lines {
        trimmed := strings.TrimSpace(line)
        if trimmed != "" {
            // Preserve relative indentation for context
            indent := len(line) - len(strings.TrimLeft(line, " \t"))
            processedLines = append(processedLines,
                strings.Repeat(" ", min(indent/2, 8)) + trimmed)
        }
    }

    // Add language context if detectable
    if lang := detectLanguage(code); lang != "" {
        return fmt.Sprintf("[%s]\n%s", lang, strings.Join(processedLines, "\n"))
    }

    return strings.Join(processedLines, "\n")
}
```

### 2. Weaviate Vector Database Integration
**Location**: `internal/query/semantic_weaviate.go`

#### Schema Design for Code Vectors

```go
// CodeChunkSchema defines the Weaviate class structure for storing code embeddings
// This schema is optimized for code search and similarity matching
var CodeChunkSchema = map[string]interface{}{
    "class": "RepoXXXXXXX",  // Dynamic class name per repository
    "description": "Code chunks with semantic embeddings for similarity search",

    // Vector configuration
    "vectorizer": "none",  // We provide pre-computed embeddings
    "moduleConfig": map[string]interface{}{
        "text2vec-transformers": map[string]interface{}{
            "skip": true,  // Skip automatic vectorization
        },
    },

    // Properties define searchable and filterable fields
    "properties": []map[string]interface{}{
        {
            "name": "repositoryId",
            "dataType": []string{"string"},
            "description": "Unique identifier for the source repository",
            "moduleConfig": map[string]interface{}{
                "text2vec-transformers": map[string]interface{}{"skip": true},
            },
        },
        {
            "name": "filePath",
            "dataType": []string{"string"},
            "description": "Relative path to the source file",
            "moduleConfig": map[string]interface{}{
                "text2vec-transformers": map[string]interface{}{"skip": true},
            },
        },
        {
            "name": "content",
            "dataType": []string{"text"},
            "description": "Raw code content for the chunk",
            "moduleConfig": map[string]interface{}{
                "text2vec-transformers": map[string]interface{}{"skip": true},
            },
        },
        {
            "name": "language",
            "dataType": []string{"string"},
            "description": "Programming language (go, javascript, python, etc)",
            "moduleConfig": map[string]interface{}{
                "text2vec-transformers": map[string]interface{}{"skip": true},
            },
        },
        {
            "name": "startLine",
            "dataType": []string{"int"},
            "description": "Starting line number in the source file",
        },
        {
            "name": "endLine",
            "dataType": []string{"int"},
            "description": "Ending line number in the source file",
        },
        {
            "name": "symbols",
            "dataType": []string{"string[]"},
            "description": "Extracted symbols (functions, classes, variables)",
            "moduleConfig": map[string]interface{}{
                "text2vec-transformers": map[string]interface{}{"skip": true},
            },
        },
    },

    // Index configuration for optimal search performance
    "invertedIndexConfig": map[string]interface{}{
        "bm25": map[string]interface{}{
            "b": 0.75,  // Term frequency saturation parameter
            "k1": 1.2,  // Term frequency scaling parameter
        },
        "stopwords": map[string]interface{}{
            "preset": "none",  // Don't filter code keywords
        },
    },

    // Vector index configuration for similarity search
    "vectorIndexConfig": map[string]interface{}{
        "ef": 64,                    // Search-time effort factor
        "efConstruction": 128,       // Build-time effort factor
        "maxConnections": 64,        // HNSW graph connectivity
        "vectorCacheMaxObjects": 500000,  // In-memory vector cache
    },
}
```

#### Similarity Search Implementation

```go
// SearchSemantic performs vector similarity search against code embeddings
// Parameters:
//   - queryEmbedding: 1536-dimensional query vector from OpenAI
//   - limit: Maximum results to return (typically 10-50)
//   - filters: Optional language, file path, or symbol filters
// Returns:
//   - Ranked code chunks with cosine similarity scores
//   - Search metadata (timing, result counts, cache status)
func (w *WeaviateClient) SearchSemantic(
    ctx context.Context,
    repositoryID string,
    queryEmbedding []float32,
    limit int,
    filters map[string]interface{},
) ([]*repocontextv1.CodeChunk, error) {
    // Validate input parameters
    if len(queryEmbedding) != 1536 {
        return nil, fmt.Errorf("invalid embedding dimension: expected 1536, got %d", len(queryEmbedding))
    }
    if limit <= 0 || limit > w.maxSearchResults {
        return nil, fmt.Errorf("invalid limit: must be 1-%d, got %d", w.maxSearchResults, limit)
    }

    className := w.getCollectionName(repositoryID)

    // Build GraphQL query for vector similarity search
    query := w.buildSemanticQuery(className, queryEmbedding, limit, filters)

    startTime := time.Now()

    // Execute GraphQL query against Weaviate
    result, err := w.client.GraphQL().Raw().WithQuery(query).Do(ctx)
    if err != nil {
        w.metrics.RecordSearchError("weaviate", err)
        return nil, fmt.Errorf("Weaviate GraphQL query failed: %w", err)
    }

    searchLatency := time.Since(startTime)

    // Parse GraphQL response
    chunks, metadata, err := w.parseSemanticSearchResponse(result, repositoryID)
    if err != nil {
        return nil, fmt.Errorf("failed to parse search response: %w", err)
    }

    // Record detailed metrics
    w.metrics.RecordBackendLatency("weaviate", searchLatency)
    w.metrics.RecordSearchResults("semantic", len(chunks))
    w.metrics.RecordVectorCacheHit(metadata.CacheHit)

    return chunks, nil
}

// buildSemanticQuery constructs GraphQL query with vector similarity search
func (w *WeaviateClient) buildSemanticQuery(
    className string,
    embedding []float32,
    limit int,
    filters map[string]interface{},
) string {
    // Convert embedding to GraphQL array format
    vectorStr := w.embeddingToGraphQL(embedding)

    // Build where clause from filters
    whereClause := w.buildWhereClause(filters)

    // Construct GraphQL query with nearVector search
    // This performs cosine similarity search in 1536-dimensional space
    query := fmt.Sprintf(`{
        Get {
            %s(
                nearVector: {
                    vector: %s
                    certainty: 0.7    # Minimum similarity threshold (0.7 = 70%% similarity)
                }
                limit: %d
                %s
            ) {
                repositoryId
                filePath
                content
                language
                startLine
                endLine
                symbols
                _additional {
                    certainty     # Cosine similarity score (0.0-1.0)
                    distance      # Vector distance (lower = more similar)
                    id            # Weaviate object ID
                    vector        # Return vector for debugging (optional)
                }
            }
        }
    }`, className, vectorStr, limit, whereClause)

    return query
}

// embeddingToGraphQL converts float32 slice to GraphQL array string
func (w *WeaviateClient) embeddingToGraphQL(embedding []float32) string {
    strValues := make([]string, len(embedding))
    for i, val := range embedding {
        strValues[i] = fmt.Sprintf("%.6f", val)  // 6 decimal precision
    }
    return "[" + strings.Join(strValues, ",") + "]"
}

// parseSemanticSearchResponse converts GraphQL response to code chunks
func (w *WeaviateClient) parseSemanticSearchResponse(
    result *graphql.Response,
    repositoryID string,
) ([]*repocontextv1.CodeChunk, *SearchMetadata, error) {
    // Navigate GraphQL response structure
    getResult, ok := result.Data["Get"].(map[string]interface{})
    if !ok {
        return nil, nil, fmt.Errorf("invalid GraphQL response format")
    }

    className := w.getCollectionName(repositoryID)
    classResults, ok := getResult[className].([]interface{})
    if !ok {
        return nil, nil, fmt.Errorf("no results found for class %s", className)
    }

    chunks := make([]*repocontextv1.CodeChunk, 0, len(classResults))
    totalDistance := float64(0)

    for rank, item := range classResults {
        result, ok := item.(map[string]interface{})
        if !ok {
            continue
        }

        // Extract additional metadata
        additional, _ := result["_additional"].(map[string]interface{})
        certainty, _ := additional["certainty"].(float64)
        distance, _ := additional["distance"].(float64)
        objectID, _ := additional["id"].(string)

        // Create code chunk with similarity score
        chunk := &repocontextv1.CodeChunk{
            RepositoryId: repositoryID,
            FilePath:     getStringField(result, "filePath"),
            Content:      getStringField(result, "content"),
            Language:     getStringField(result, "language"),
            StartLine:    int32(getIntField(result, "startLine")),
            EndLine:      int32(getIntField(result, "endLine")),
            Score:        float32(certainty),  // Use certainty as relevance score
            Source:       repocontextv1.SearchSource_SEARCH_SOURCE_SEMANTIC,

            // Additional semantic search metadata
            Metadata: &repocontextv1.ChunkMetadata{
                WeaviateObjectId: objectID,
                VectorDistance:   float32(distance),
                SearchRank:      int32(rank + 1),
            },
        }

        // Extract symbols array
        if symbolsInterface, exists := result["symbols"]; exists {
            if symbolsArray, ok := symbolsInterface.([]interface{}); ok {
                symbols := make([]string, len(symbolsArray))
                for i, sym := range symbolsArray {
                    symbols[i] = fmt.Sprintf("%v", sym)
                }
                chunk.Symbols = symbols
            }
        }

        chunks = append(chunks, chunk)
        totalDistance += distance
    }

    // Calculate search metadata
    metadata := &SearchMetadata{
        ResultCount:    len(chunks),
        AverageScore:   float32(1.0 - (totalDistance / float64(len(chunks)))),  // Convert distance to similarity
        QueryLatency:   0,  // Set by caller
        CacheHit:       false,  // Determined by Weaviate internal cache
    }

    return chunks, metadata, nil
}
```

### 3. Query Processing and Embedding Generation

#### Query Preprocessing for Better Results

```go
// generateQueryEmbedding creates optimized embeddings for search queries
// This function is critical for search quality - poor query embeddings = poor results
func (s *ChatServer) generateQueryEmbedding(ctx context.Context, queryText string) ([]float32, error) {
    // Clean and normalize the query for better embedding quality
    processedQuery := s.preprocessQuery(queryText)

    // Generate embedding using the same model as content embeddings
    embeddings, err := s.embeddingClient.GenerateEmbeddings(
        ctx,
        []string{processedQuery},
        "text-embedding-ada-002",  // Must match content embedding model
    )
    if err != nil {
        return nil, fmt.Errorf("failed to generate query embedding: %w", err)
    }

    if len(embeddings) == 0 || len(embeddings[0]) == 0 {
        return nil, fmt.Errorf("received empty embedding for query")
    }

    embedding := embeddings[0]

    // Optional: Normalize embedding vector for consistent similarity calculation
    if s.config.NormalizeEmbeddings {
        embedding = normalizeVector(embedding)
    }

    return embedding, nil
}

// preprocessQuery optimizes natural language queries for code search
func (s *ChatServer) preprocessQuery(query string) string {
    // Convert natural language to code-focused terms
    codeTerms := map[string][]string{
        // Authentication patterns
        "authentication": {"auth", "login", "signin", "authenticate", "verify"},
        "authorization": {"auth", "authz", "permission", "access", "role"},

        // API patterns
        "api endpoint": {"route", "handler", "endpoint", "controller"},
        "http request": {"request", "req", "http", "method"},

        // Database patterns
        "database": {"db", "sql", "query", "model", "entity"},
        "data model": {"struct", "class", "schema", "model"},

        // Error patterns
        "error handling": {"error", "err", "exception", "try", "catch"},
    }

    processedQuery := strings.ToLower(query)

    // Expand query with related code terms
    for phrase, terms := range codeTerms {
        if strings.Contains(processedQuery, phrase) {
            // Add related terms to improve semantic matching
            processedQuery += " " + strings.Join(terms, " ")
        }
    }

    // Add programming context clues
    if !containsCodeTerms(processedQuery) {
        processedQuery = "code programming " + processedQuery
    }

    return processedQuery
}

// normalizeVector converts embedding to unit vector for consistent similarity scores
func normalizeVector(vector []float32) []float32 {
    var sumSquares float32
    for _, val := range vector {
        sumSquares += val * val
    }

    magnitude := float32(math.Sqrt(float64(sumSquares)))
    if magnitude == 0 {
        return vector  // Avoid division by zero
    }

    normalized := make([]float32, len(vector))
    for i, val := range vector {
        normalized[i] = val / magnitude
    }

    return normalized
}
```

## Performance Optimization Strategies

### 1. Vector Index Configuration

**Weaviate HNSW (Hierarchical Navigable Small World) Parameters:**

```yaml
vectorIndexConfig:
  ef: 64                    # Search-time effort (higher = better recall, slower search)
  efConstruction: 128       # Build-time effort (higher = better index, slower build)
  maxConnections: 64        # Node connectivity (higher = better recall, more memory)
  vectorCacheMaxObjects: 500000  # In-memory cache size
```

**Performance Impact Analysis:**
- **ef=64**: ~100ms search latency, 85% recall
- **ef=128**: ~200ms search latency, 92% recall
- **ef=256**: ~400ms search latency, 96% recall

### 2. Embedding Batch Processing

```go
// BatchEmbeddingProcessor optimizes OpenAI API usage with intelligent batching
type BatchEmbeddingProcessor struct {
    client       *OpenAIEmbeddingClient
    batchSize    int           // Optimal: 50-100 texts per batch
    maxTokens    int           // OpenAI limit: 2048 tokens per text
    rateLimiter  *rate.Limiter // Rate limiting for API calls
}

func (bp *BatchEmbeddingProcessor) ProcessChunks(chunks []*CodeChunk) ([][]float32, error) {
    // Group chunks into optimal batches
    batches := bp.createOptimalBatches(chunks)
    embeddings := make([][]float32, len(chunks))

    for batchIdx, batch := range batches {
        // Wait for rate limiter
        if err := bp.rateLimiter.Wait(context.Background()); err != nil {
            return nil, err
        }

        // Process batch
        batchEmbeddings, err := bp.client.GenerateEmbeddings(
            context.Background(),
            extractTexts(batch),
            "text-embedding-ada-002",
        )
        if err != nil {
            // Implement exponential backoff for rate limit errors
            if isRateLimitError(err) {
                time.Sleep(calculateBackoff(batchIdx))
                continue  // Retry this batch
            }
            return nil, err
        }

        // Copy batch results to final array
        copy(embeddings[batchIdx*bp.batchSize:], batchEmbeddings)
    }

    return embeddings, nil
}

// createOptimalBatches groups chunks by token count and content similarity
func (bp *BatchEmbeddingProcessor) createOptimalBatches(chunks []*CodeChunk) [][]ChunkText {
    batches := [][]ChunkText{}
    currentBatch := []ChunkText{}
    currentTokens := 0

    for _, chunk := range chunks {
        tokens := estimateTokenCount(chunk.Content)

        // Check if adding this chunk would exceed limits
        if len(currentBatch) >= bp.batchSize ||
           currentTokens+tokens > bp.maxTokens*bp.batchSize {
            // Start new batch
            if len(currentBatch) > 0 {
                batches = append(batches, currentBatch)
                currentBatch = []ChunkText{}
                currentTokens = 0
            }
        }

        currentBatch = append(currentBatch, ChunkText{
            Content: chunk.Content,
            ChunkID: chunk.ID,
        })
        currentTokens += tokens
    }

    // Add final batch
    if len(currentBatch) > 0 {
        batches = append(batches, currentBatch)
    }

    return batches
}
```

### 3. Search Result Caching

```go
// SemanticSearchCache implements intelligent caching for vector search results
type SemanticSearchCache struct {
    redis       *redis.Client
    keyPrefix   string
    defaultTTL  time.Duration
}

// CacheKey generates consistent cache keys for search queries
func (c *SemanticSearchCache) CacheKey(repositoryID, queryHash string, filters map[string]interface{}) string {
    // Include filter hash to avoid cache pollution
    filterHash := hashFilters(filters)
    return fmt.Sprintf("%s:semantic:%s:%s:%s", c.keyPrefix, repositoryID, queryHash, filterHash)
}

// Get retrieves cached search results with embedding similarity verification
func (c *SemanticSearchCache) Get(
    ctx context.Context,
    cacheKey string,
    queryEmbedding []float32,
) ([]*repocontextv1.CodeChunk, bool) {
    // Retrieve cached data
    cached, err := c.redis.Get(ctx, cacheKey).Result()
    if err == redis.Nil {
        return nil, false  // Cache miss
    }
    if err != nil {
        log.Printf("Cache read error: %v", err)
        return nil, false
    }

    // Deserialize cached results
    var cachedResults CachedSearchResults
    if err := json.Unmarshal([]byte(cached), &cachedResults); err != nil {
        return nil, false
    }

    // Verify embedding similarity to detect cache staleness
    // This prevents returning stale results for similar but different queries
    similarity := cosineSimilarity(queryEmbedding, cachedResults.QueryEmbedding)
    if similarity < 0.95 {  // 95% similarity threshold
        return nil, false  // Cache miss due to low similarity
    }

    return cachedResults.Results, true
}

// Set stores search results with metadata for intelligent invalidation
func (c *SemanticSearchCache) Set(
    ctx context.Context,
    cacheKey string,
    results []*repocontextv1.CodeChunk,
    queryEmbedding []float32,
    searchMetadata *SearchMetadata,
) error {
    cachedResults := CachedSearchResults{
        Results:        results,
        QueryEmbedding: queryEmbedding,
        CachedAt:      time.Now(),
        Metadata:      searchMetadata,
    }

    data, err := json.Marshal(cachedResults)
    if err != nil {
        return err
    }

    // Dynamic TTL based on search quality and result count
    ttl := c.calculateTTL(searchMetadata)

    return c.redis.SetEX(ctx, cacheKey, data, ttl).Err()
}

// calculateTTL adjusts cache lifetime based on search quality
func (c *SemanticSearchCache) calculateTTL(metadata *SearchMetadata) time.Duration {
    baseTTL := c.defaultTTL

    // High-quality results cache longer
    if metadata.AverageScore > 0.8 {
        baseTTL *= 2  // 2x TTL for high-confidence results
    }

    // Many results suggest comprehensive query - cache longer
    if metadata.ResultCount > 20 {
        baseTTL *= 1.5
    }

    // Fast queries (likely cached internally) can cache longer
    if metadata.QueryLatency < 50*time.Millisecond {
        baseTTL *= 1.2
    }

    return baseTTL
}
```

## Query Types and Optimization

### 1. Natural Language Queries

**Example**: "How does authentication work in this codebase?"

**Processing Pipeline**:
```
1. Query Preprocessing:
   "How does authentication work in this codebase?"
   → "authentication auth login signin verify code programming"

2. Embedding Generation:
   → [0.123, -0.456, 0.789, ...] (1536 dimensions)

3. Vector Search:
   → Cosine similarity against code embeddings
   → Similarity threshold: 0.7 (70%)

4. Result Ranking:
   → Primary: Cosine similarity score
   → Secondary: Code quality heuristics
   → Tertiary: File type preferences
```

### 2. Code Structure Queries

**Example**: "Find all API endpoints"

**Optimized Processing**:
```go
// StructuralQueryOptimizer enhances queries for code structure searches
func (s *SemanticSearchService) optimizeStructuralQuery(query string) (*OptimizedQuery, error) {
    patterns := map[string]StructurePattern{
        "api endpoints": {
            Terms:    []string{"route", "handler", "endpoint", "controller", "api"},
            Filters:  map[string]interface{}{"symbols": ["function", "method"]},
            Boost:    1.5,  // Increase relevance for structural matches
        },
        "database models": {
            Terms:   []string{"model", "schema", "entity", "struct", "class"},
            Filters: map[string]interface{}{"language": ["go", "python", "java"]},
            Boost:   1.3,
        },
        "error handling": {
            Terms:   []string{"error", "exception", "err", "try", "catch"},
            Filters: map[string]interface{}{"symbols": ["error", "exception"]},
            Boost:   1.2,
        },
    }

    for pattern, config := range patterns {
        if strings.Contains(strings.ToLower(query), pattern) {
            return &OptimizedQuery{
                EnhancedTerms: append([]string{query}, config.Terms...),
                Filters:       config.Filters,
                ScoreBoost:    config.Boost,
                SearchHint:    pattern,
            }, nil
        }
    }

    // Default processing for unmatched queries
    return &OptimizedQuery{
        EnhancedTerms: []string{query},
        ScoreBoost:    1.0,
    }, nil
}
```

## Similarity Scoring Deep Dive

### Cosine Similarity Mathematics

```go
// cosineSimilarity calculates the cosine of the angle between two vectors
// Score range: -1.0 (opposite) to 1.0 (identical)
// Typical code similarity scores: 0.3-0.9
func cosineSimilarity(vecA, vecB []float32) float32 {
    if len(vecA) != len(vecB) {
        return 0.0
    }

    var dotProduct, normA, normB float64

    // Calculate dot product and norms simultaneously
    for i := range vecA {
        a, b := float64(vecA[i]), float64(vecB[i])
        dotProduct += a * b
        normA += a * a
        normB += b * b
    }

    // Handle zero vectors
    if normA == 0.0 || normB == 0.0 {
        return 0.0
    }

    // Cosine similarity = dot(A,B) / (||A|| * ||B||)
    similarity := dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))

    return float32(similarity)
}

// interpretSimilarityScore provides human-readable similarity assessment
func interpretSimilarityScore(score float32) string {
    switch {
    case score >= 0.9:  return "Nearly identical"    // Rare, usually duplicates
    case score >= 0.8:  return "Highly similar"      // Strong conceptual match
    case score >= 0.7:  return "Similar"             // Good relevance
    case score >= 0.6:  return "Somewhat similar"    // Moderate relevance
    case score >= 0.5:  return "Weakly similar"      // Low relevance
    default:           return "Dissimilar"           // Poor match
    }
}
```

### Advanced Scoring Strategies

```go
// SemanticScorer implements multi-factor scoring for better result ranking
type SemanticScorer struct {
    config ScoringConfig
}

type ScoringConfig struct {
    VectorWeight      float32  // Base cosine similarity weight (0.6)
    LanguageBoost     float32  // Same language preference (0.1)
    FreshnessBoost    float32  // Newer code preference (0.05)
    PopularityBoost   float32  // Frequently accessed code (0.05)
    SymbolMatchBoost  float32  // Symbol name matches (0.2)
}

// CalculateEnhancedScore combines multiple relevance signals
func (s *SemanticScorer) CalculateEnhancedScore(
    chunk *repocontextv1.CodeChunk,
    query string,
    baseScore float32,
) float32 {
    score := baseScore * s.config.VectorWeight

    // Language matching boost
    if s.matchesQueryLanguage(chunk, query) {
        score += s.config.LanguageBoost
    }

    // Symbol matching boost
    symbolBoost := s.calculateSymbolMatchScore(chunk.Symbols, query)
    score += symbolBoost * s.config.SymbolMatchBoost

    // Code quality indicators
    qualityScore := s.assessCodeQuality(chunk.Content)
    score *= (1.0 + qualityScore*0.1)  // Up to 10% boost for high quality

    // Freshness boost (if timestamp available)
    if chunk.Metadata != nil && chunk.Metadata.LastModified != nil {
        freshnessBoost := s.calculateFreshnessScore(chunk.Metadata.LastModified)
        score += freshnessBoost * s.config.FreshnessBoost
    }

    // Normalize to [0, 1] range
    return min(score, 1.0)
}

// calculateSymbolMatchScore rewards chunks containing query-related symbols
func (s *SemanticScorer) calculateSymbolMatchScore(symbols []string, query string) float32 {
    if len(symbols) == 0 {
        return 0.0
    }

    queryTerms := strings.Fields(strings.ToLower(query))
    matchCount := 0

    for _, symbol := range symbols {
        symbolLower := strings.ToLower(symbol)
        for _, term := range queryTerms {
            if strings.Contains(symbolLower, term) || strings.Contains(term, symbolLower) {
                matchCount++
                break
            }
        }
    }

    return float32(matchCount) / float32(len(symbols))
}

// assessCodeQuality provides heuristic code quality scoring
func (s *SemanticScorer) assessCodeQuality(content string) float32 {
    lines := strings.Split(content, "\n")

    var qualityScore float32 = 0.5  // Base score

    // Positive indicators
    hasComments := 0
    hasTests := 0
    properFunctions := 0

    for _, line := range lines {
        line = strings.TrimSpace(line)

        // Comment presence
        if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/*") {
            hasComments++
        }

        // Test indicators
        if strings.Contains(strings.ToLower(line), "test") || strings.Contains(line, "assert") {
            hasTests++
        }

        // Well-structured functions
        if strings.Contains(line, "func ") || strings.Contains(line, "function ") || strings.Contains(line, "def ") {
            properFunctions++
        }
    }

    lineCount := float32(len(lines))

    // Calculate quality components
    commentRatio := min(float32(hasComments)/lineCount, 0.3)  // Up to 30% comments is good
    testPresence := min(float32(hasTests)/lineCount, 0.1)     // Some test indicators
    structureScore := min(float32(properFunctions)/lineCount, 0.2)  // Well-defined functions

    qualityScore += commentRatio + testPresence + structureScore

    // Penalties for poor quality indicators
    if lineCount > 100 && hasComments == 0 {
        qualityScore -= 0.2  // Large uncommented blocks
    }

    return max(0.0, min(1.0, qualityScore))
}
```

## Integration with Dual Search System

The semantic search integrates with lexical search (ripgrep) through the result merging system:

```go
// Dual search coordination from internal/api/chat.go
func (s *ChatServer) performDualSearch(
    ctx context.Context,
    repositoryID, queryText string,
    limit int32,
) ([]*repocontextv1.CodeChunk, error) {
    // Parallel execution using goroutines
    lexicalChan := make(chan []*repocontextv1.CodeChunk, 1)
    semanticChan := make(chan []*repocontextv1.CodeChunk, 1)

    // Goroutine 1: Lexical search (exact/fuzzy text matching)
    go func() {
        results, _ := s.queryService.lexicalClient.SearchLexical(ctx, repositoryID, queryText, int(limit), nil)
        lexicalChan <- results
    }()

    // Goroutine 2: Semantic search (vector similarity)
    go func() {
        queryEmbedding, err := s.generateQueryEmbedding(ctx, queryText)
        if err != nil {
            semanticChan <- nil
            return
        }
        results, _ := s.queryService.semanticClient.SearchSemantic(ctx, repositoryID, queryEmbedding, int(limit), nil)
        semanticChan <- results
    }()

    // Collect results from both backends
    lexicalResults := <-lexicalChan
    semanticResults := <-semanticChan

    // Merge and rank combined results
    merged := s.queryService.merger.MergeAndRank(&query.SearchResults{
        LexicalChunks:  lexicalResults,   // Exact matches, regex patterns
        SemanticChunks: semanticResults,  // Conceptual similarities
    })

    return merged.Chunks, nil
}
```

This dual approach ensures both precise exact matches (lexical) and conceptual understanding (semantic), providing comprehensive code search capabilities that adapt to different query types and user intents.