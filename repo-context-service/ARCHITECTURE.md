# Architecture Deep Dive

This document provides an in-depth technical analysis of the Repository Context + Coding Agent Service architecture, focusing on component integrations, concurrency patterns, and protocol trade-offs.

## System Overview

The service implements a **gRPC-first architecture** with multiple protocol bridges to support different client types while maintaining high performance and type safety.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                          Client Layer                                        │
├─────────────────────────┬─────────────────────┬─────────────────────────────┤
│     Web Browser         │    gRPC Clients     │    HTTP/REST Clients        │
│   (JavaScript/WS)       │  (Go, Python, etc)  │   (curl, Postman, etc)      │
└─────────────────────────┼─────────────────────┼─────────────────────────────┘
                          │                     │
┌─────────────────────────┼─────────────────────┼─────────────────────────────┐
│                    Protocol Bridge Layer                                     │
├─────────────────────────┼─────────────────────┼─────────────────────────────┤
│  WebSocket Bridge       │                     │   gRPC-Gateway              │
│  (Real-time Chat)       │   Native gRPC       │   (HTTP/JSON → gRPC)        │
│  JSON ↔ Protobuf        │   (Protobuf)        │   REST → RPC                │
└─────────────────────────┼─────────────────────┼─────────────────────────────┘
                          │                     │
                          └─────────┬───────────┘
                                    │
┌─────────────────────────────────────────────────────────────────────────────┐
│                        Core gRPC Services                                    │
├─────────────────┬─────────────────┬─────────────────┬─────────────────────────┤
│ UploadService   │RepositoryService│  ChatService    │   HealthService         │
│ - Git clone     │ - List repos    │ - Dual search   │ - Health checks         │
│ - File process  │ - Get details   │ - LLM compose   │ - Dependency status     │
│ - Status track  │ - Delete repos  │ - Stream chat   │                         │
└─────────────────┼─────────────────┼─────────────────┼─────────────────────────┘
                  │                 │                 │
┌─────────────────┼─────────────────┼─────────────────┼─────────────────────────┐
│                           Business Logic Layer                               │
├─────────────────┼─────────────────┼─────────────────┼─────────────────────────┤
│ IngestProvider  │   QueryService  │  ChatServer     │    CacheManager         │
│ - File parsing  │ - Search coord  │ - Session mgmt  │  - Redis operations     │
│ - Chunking      │ - Result merge  │ - Streaming     │  - TTL management       │
│ - Embedding     │ - Score ranking │ - Concurrency   │  - Invalidation         │
└─────────────────┼─────────────────┼─────────────────┼─────────────────────────┘
                  │                 │                 │
┌─────────────────┼─────────────────┼─────────────────┼─────────────────────────┐
│                          Data Access Layer                                   │
├─────────────────┼─────────────────┼─────────────────┼─────────────────────────┤
│  File System    │ Semantic Search │  Lexical Search │      Cache Layer        │
│  - Repository   │ - Weaviate DB   │ - ripgrep exec  │    - Redis KV           │
│  - Git ops      │ - Vector store  │ - Regex match   │    - Session store      │
│  - Temp files   │ - Similarity    │ - File filter   │    - Query cache        │
└─────────────────┼─────────────────┼─────────────────┼─────────────────────────┘
                  │                 │                 │
┌─────────────────┼─────────────────┼─────────────────┼─────────────────────────┐
│                        External Services                                     │
├─────────────────┼─────────────────┼─────────────────┼─────────────────────────┤
│   OpenAI API    │   Weaviate DB   │   File System   │     DeepSeek API        │
│ - Embeddings    │ - Vector ops    │ - Git repos     │   - LLM generation      │
│ - Rate limits   │ - Collections   │ - Temp storage  │   - Token streaming     │
│ - Batch proc    │ - Health check  │ - Cleanup       │   - Context limits      │
└─────────────────┴─────────────────┴─────────────────┴─────────────────────────┘
```

## Protocol Bridges and Trade-offs

### HTTP vs gRPC: Design Decisions

#### gRPC Advantages (Why we chose gRPC-first)
- **Type Safety**: Protocol Buffers provide compile-time type checking
- **Performance**: Binary serialization ~30% faster than JSON
- **Streaming**: Native bidirectional streaming for real-time chat
- **Consistency**: Uniform API contracts across all services
- **Code Generation**: Automatic client libraries in multiple languages

#### HTTP/REST Advantages (Why we provide HTTP gateway)
- **Browser Compatibility**: JavaScript can't speak gRPC directly
- **Tooling**: curl, Postman, browser dev tools work out of the box
- **Debugging**: Human-readable JSON for development
- **Caching**: HTTP caching headers and proxies

#### Implementation Trade-offs

| Aspect | gRPC Native | HTTP Gateway | WebSocket Bridge |
|--------|-------------|--------------|------------------|
| **Latency** | ~2ms | ~5-8ms | ~3-5ms |
| **Consistency** | High (Protobuf) | Medium (JSON mapping) | High (Direct bridge) |
| **Availability** | Service mesh | HTTP load balancers | WebSocket-aware LBs |
| **Debugging** | grpcurl, logs | curl, browser | Browser dev tools |
| **Streaming** | Native | Server-sent events | Native WebSocket |

### WebSocket ↔ gRPC Bridge: Deep Dive

**Location**: `internal/api/websocket.go`

The WebSocket bridge enables real-time chat while maintaining gRPC's type safety and streaming benefits.

#### Connection Lifecycle
```go
// Connection establishment - cmd/apiserver/main.go:260
func (h *ChatWebSocketHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
    // 1. HTTP → WebSocket upgrade (Gorilla WebSocket)
    conn, err := h.upgrader.Upgrade(w, r, nil)

    // 2. Create unique connection ID for session management
    connID := fmt.Sprintf("%s_%d", repositoryID, time.Now().UnixNano())

    // 3. Register connection for cleanup tracking
    h.connMutex.Lock()
    h.connections[connID] = conn
    h.connMutex.Unlock()

    // 4. Establish gRPC client connection to internal service
    grpcConn, err := grpc.DialContext(ctx, "localhost:9090", grpc.WithInsecure())
    client := repocontextv1.NewChatServiceClient(grpcConn)
    stream, err := client.ChatWithRepository(ctx)

    // 5. Start bidirectional bridge goroutines
    done := make(chan bool)
    go h.grpcToWebSocket(stream, conn, done)  // gRPC → WebSocket
    h.webSocketToGRPC(conn, stream, repositoryID, done)  // WebSocket → gRPC
}
```

#### Concurrency Pattern: Bidirectional Streaming

**Goroutine 1**: WebSocket → gRPC (Message Ingestion)
```go
func (h *ChatWebSocketHandler) webSocketToGRPC(
    wsConn *websocket.Conn,
    grpcStream repocontextv1.ChatService_ChatWithRepositoryClient,
    done chan bool,
) {
    defer func() { done <- true }()  // Signal completion

    for {
        // Read JSON from WebSocket
        var wsMsg WSMessage
        err := wsConn.ReadJSON(&wsMsg)
        if err != nil {
            // Handle connection closure gracefully
            if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
                log.Printf("WebSocket read error: %v", err)
            }
            return
        }

        // Convert JSON → Protobuf with type mapping
        grpcReq := h.convertWebSocketToGRPC(wsMsg)

        // Send to gRPC stream (non-blocking)
        err = grpcStream.Send(grpcReq)
        if err != nil {
            log.Printf("gRPC send error: %v", err)
            return
        }
    }
}
```

**Goroutine 2**: gRPC → WebSocket (Response Streaming)
```go
func (h *ChatWebSocketHandler) grpcToWebSocket(
    grpcStream repocontextv1.ChatService_ChatWithRepositoryClient,
    wsConn *websocket.Conn,
    done chan bool,
) {
    defer func() { done <- true }()  // Signal completion

    for {
        select {
        case <-done:
            return  // Graceful shutdown on peer closure
        default:
            // Read Protobuf from gRPC stream
            grpcResp, err := grpcStream.Recv()
            if err != nil {
                log.Printf("gRPC receive error: %v", err)
                return
            }

            // Convert Protobuf → JSON with field mapping
            wsResp := h.convertGRPCToWebSocket(grpcResp)

            // Send JSON to WebSocket (non-blocking)
            err = wsConn.WriteJSON(wsResp)
            if err != nil {
                log.Printf("WebSocket write error: %v", err)
                return
            }
        }
    }
}
```

#### Protocol Translation Layer

**Key Design Decision**: Maintain protocol isolation while ensuring semantic consistency.

```go
// JSON WebSocket message structure (external API)
type WSChatMessage struct {
    Query     string `json:"query"`
    SessionID string `json:"session_id"`
}

// Protobuf gRPC message (internal API) - auto-generated
type ChatMessage struct {
    Query     string `protobuf:"bytes,1,opt,name=query,proto3"`
    SessionId string `protobuf:"bytes,2,opt,name=session_id,json=sessionId,proto3"`
}

// Translation maintains field semantics while adapting naming conventions
func (h *ChatWebSocketHandler) convertWebSocketToGRPC(wsMsg WSMessage) *repocontextv1.ChatRequest {
    if wsMsg.ChatMessage != nil {
        return &repocontextv1.ChatRequest{
            Message: &repocontextv1.ChatRequest_ChatMessage{
                ChatMessage: &repocontextv1.ChatMessage{
                    Query:     wsMsg.ChatMessage.Query,
                    SessionId: wsMsg.ChatMessage.SessionID,  // snake_case → camelCase
                },
            },
        }
    }
    // ... handle other message types
}
```

## Dual Search Architecture: Concurrency Deep Dive

**Location**: `internal/api/chat.go:performDualSearch`

The dual search system demonstrates advanced Go concurrency patterns for coordinating multiple backends.

### Parallel Search Execution

```go
// performDualSearch coordinates lexical and semantic search concurrently
func (s *ChatServer) performDualSearch(ctx context.Context, repositoryID, queryText string, limit int32) ([]*repocontextv1.CodeChunk, error) {
    // Channel-based coordination for concurrent operations
    lexicalChan := make(chan searchResult, 1)
    semanticChan := make(chan searchResult, 1)
    embeddingChan := make(chan embeddingResult, 1)

    // Goroutine 1: Lexical search (ripgrep) - CPU intensive
    go func() {
        defer close(lexicalChan)
        results, err := s.queryService.lexicalClient.SearchLexical(
            ctx, repositoryID, queryText, int(limit), nil)
        lexicalChan <- searchResult{results: results, err: err}
    }()

    // Goroutine 2: Query embedding generation - I/O intensive (OpenAI API)
    go func() {
        defer close(embeddingChan)
        embedding, err := s.generateQueryEmbedding(ctx, queryText)
        embeddingChan <- embeddingResult{embedding: embedding, err: err}
    }()

    // Wait for query embedding before starting semantic search
    embeddingRes := <-embeddingChan
    if embeddingRes.err != nil {
        return nil, fmt.Errorf("failed to generate query embedding: %w", embeddingRes.err)
    }

    // Goroutine 3: Semantic search (Weaviate) - Network intensive
    go func() {
        defer close(semanticChan)
        results, err := s.queryService.semanticClient.SearchSemantic(
            ctx, repositoryID, embeddingRes.embedding, int(limit), nil)
        semanticChan <- searchResult{results: results, err: err}
    }()

    // Coordinate result collection with timeout handling
    lexicalRes := <-lexicalChan
    semanticRes := <-semanticChan

    // Handle partial failures gracefully
    if lexicalRes.err != nil && semanticRes.err != nil {
        return nil, fmt.Errorf("both search backends failed: lexical=%v, semantic=%v",
                              lexicalRes.err, semanticRes.err)
    }

    // Merge results even if one backend failed
    mergedResults := s.queryService.merger.MergeAndRank(&query.SearchResults{
        LexicalChunks:  lexicalRes.results,
        SemanticChunks: semanticRes.results,
    })

    return mergedResults.Chunks[:min(int(limit), len(mergedResults.Chunks))], nil
}
```

### Search Result Merging: Advanced Algorithms

**Location**: `internal/query/merge.go`

The result merger implements sophisticated ranking and deduplication algorithms:

#### Score Normalization (Z-Score + Sigmoid)
```go
// normalizeScores applies statistical normalization to make scores comparable
func (rm *ResultMerger) normalizeScores(chunks []*repocontextv1.CodeChunk) []*repocontextv1.CodeChunk {
    // Calculate population statistics
    var sum, sumSquares float64
    for _, chunk := range chunks {
        score := float64(chunk.Score)
        sum += score
        sumSquares += score * score
    }

    count := float64(len(chunks))
    mean := sum / count
    variance := (sumSquares / count) - (mean * mean)
    stdDev := math.Sqrt(variance)

    // Apply z-score normalization + sigmoid mapping to [0,1]
    for i, chunk := range chunks {
        zScore := (float64(chunk.Score) - mean) / stdDev
        normalizedScore := 1.0 / (1.0 + math.Exp(-zScore))  // Sigmoid function
        chunk.Score = float32(normalizedScore)
    }
}
```

#### Overlap Detection and Chunk Merging
```go
// hasOverlap detects nearby code chunks that should be merged
func (rm *ResultMerger) hasOverlap(chunk1, chunk2 *repocontextv1.CodeChunk) bool {
    if chunk1.FilePath != chunk2.FilePath {
        return false
    }

    // Merge if within 5 lines (configurable proximity threshold)
    return (chunk1.EndLine >= chunk2.StartLine-5) && (chunk1.StartLine <= chunk2.EndLine+5)
}

// mergeOverlappingChunks combines related code sections intelligently
func (rm *ResultMerger) mergeOverlappingChunks(chunk1, chunk2 *repocontextv1.CodeChunk) *repocontextv1.CodeChunk {
    merged := rm.copyChunk(chunk1)

    // Expand line range to encompass both chunks
    merged.StartLine = min(chunk1.StartLine, chunk2.StartLine)
    merged.EndLine = max(chunk1.EndLine, chunk2.EndLine)

    // Intelligent content merging (avoid duplication)
    if !strings.Contains(merged.Content, chunk2.Content) {
        merged.Content = rm.combineContent(merged.Content, chunk2.Content)
    }

    // Use maximum relevance score
    merged.Score = max(chunk1.Score, chunk2.Score)

    // Mark as merged for tracking
    merged.Source = repocontextv1.SearchSource_SEARCH_SOURCE_MERGED

    return merged
}
```

## Repository Ingestion Pipeline: State Machine

**Location**: `internal/ingest/inline.go`

The ingestion pipeline implements a robust state machine for processing repositories with proper error handling and progress tracking.

### State Transitions
```
STATE_PENDING ─[Git Clone]──→ STATE_EXTRACTING
      │                              │
      │                              │ [File Discovery]
      ▼                              ▼
STATE_FAILED ←──[Error]──── STATE_CHUNKING
      ▲                              │
      │                              │ [Content Segmentation]
      │                              ▼
      └───[Error]────────── STATE_EMBEDDING
                                     │
                                     │ [OpenAI API Calls]
                                     ▼
                            STATE_INDEXING
                                     │
                                     │ [Weaviate Storage]
                                     ▼
                             STATE_READY
```

### Concurrent Processing with Semaphore Pattern

```go
// ProcessRepositoryInline coordinates the complete pipeline with resource limits
func (p *InlineProcessor) ProcessRepositoryInline(
    ctx context.Context,
    upload *repocontextv1.RepositoryUpload,
    callback func(*repocontextv1.RepositoryStatus),
) error {
    // Semaphore pattern for resource management
    select {
    case p.processingChan <- struct{}{}:  // Acquire processing slot
        defer func() { <-p.processingChan }()  // Release slot on completion
    case <-ctx.Done():
        return ctx.Err()
    }

    // State progression with checkpoint persistence
    stages := []struct {
        name  string
        state repocontextv1.RepositoryState
        fn    func(context.Context, *processingContext) error
    }{
        {"clone", repocontextv1.RepositoryState_STATE_EXTRACTING, p.cloneRepository},
        {"chunk", repocontextv1.RepositoryState_STATE_CHUNKING, p.chunkFiles},
        {"embed", repocontextv1.RepositoryState_STATE_EMBEDDING, p.generateEmbeddings},
        {"index", repocontextv1.RepositoryState_STATE_INDEXING, p.indexEmbeddings},
    }

    for i, stage := range stages {
        // Update state before processing (checkpoint pattern)
        status := &repocontextv1.RepositoryStatus{
            State:    stage.state,
            Progress: int32((i * 100) / len(stages)),
        }
        callback(status)

        // Execute stage with timeout and error recovery
        stageCtx, cancel := context.WithTimeout(ctx, p.config.StageTimeout)
        err := stage.fn(stageCtx, processingCtx)
        cancel()

        if err != nil {
            // Log error details for debugging
            log.Printf("Stage %s failed for upload %s: %v", stage.name, upload.UploadId, err)

            // Update to failed state with error details
            failedStatus := &repocontextv1.RepositoryStatus{
                State:   repocontextv1.RepositoryState_STATE_FAILED,
                Error:   err.Error(),
                Progress: int32((i * 100) / len(stages)),
            }
            callback(failedStatus)
            return fmt.Errorf("stage %s failed: %w", stage.name, err)
        }
    }

    // Final success state
    callback(&repocontextv1.RepositoryStatus{
        State:    repocontextv1.RepositoryState_STATE_READY,
        Progress: 100,
    })

    return nil
}
```

### Embedding Generation: Batch Processing with Backpressure

```go
// generateEmbeddings implements batch processing with OpenAI API rate limiting
func (p *InlineProcessor) generateEmbeddings(ctx context.Context, pCtx *processingContext) error {
    const batchSize = 50  // OpenAI API optimal batch size
    const maxRetries = 3
    const backoffBase = time.Second

    chunks := pCtx.chunks
    embeddings := make([][]float32, len(chunks))

    // Process in batches with exponential backoff
    for i := 0; i < len(chunks); i += batchSize {
        end := min(i+batchSize, len(chunks))
        batch := chunks[i:end]

        // Extract text for embedding generation
        texts := make([]string, len(batch))
        for j, chunk := range batch {
            texts[j] = chunk.Content
        }

        // Retry loop with exponential backoff
        var batchEmbeddings [][]float32
        var err error

        for attempt := 0; attempt < maxRetries; attempt++ {
            batchEmbeddings, err = p.embeddingClient.GenerateEmbeddings(
                ctx, texts, "text-embedding-ada-002")

            if err == nil {
                break  // Success
            }

            // Rate limit handling
            if strings.Contains(err.Error(), "rate_limit_exceeded") {
                backoffDuration := backoffBase * time.Duration(1<<attempt)
                log.Printf("Rate limited, backing off for %v", backoffDuration)

                select {
                case <-time.After(backoffDuration):
                    continue  // Retry after backoff
                case <-ctx.Done():
                    return ctx.Err()
                }
            }

            // Other errors are not retryable
            return fmt.Errorf("embedding generation failed after %d attempts: %w", attempt+1, err)
        }

        // Store batch results
        copy(embeddings[i:], batchEmbeddings)

        // Progress callback
        progress := int32((i * 100) / len(chunks))
        if progress%10 == 0 {  // Update every 10%
            status := &repocontextv1.RepositoryStatus{
                State:    repocontextv1.RepositoryState_STATE_EMBEDDING,
                Progress: 40 + progress/2,  // Embedding is 40-80% of total progress
            }
            p.statusCallback(status)
        }
    }

    pCtx.embeddings = embeddings
    return nil
}
```

## Caching Strategy: Multi-Layer with TTL Management

**Location**: `internal/cache/redis.go`

The caching system implements sophisticated TTL management and cache invalidation strategies.

### Cache Key Hierarchy
```
tenant:{tenant_id}:upload:{upload_id}            (TTL: 24h)
tenant:{tenant_id}:repository:{repo_id}          (TTL: 7d)
tenant:{tenant_id}:query:{query_hash}:{repo_id}  (TTL: 1h)
tenant:{tenant_id}:routing:{repo_id}             (TTL: 24h)
```

### Intelligent Cache Warming
```go
// WarmCache pre-loads frequently accessed data to reduce latency
func (c *RedisCache) WarmCache(ctx context.Context, repoID string) error {
    // Pre-load repository metadata
    repoKey := c.buildKey("repository", repoID)
    exists, err := c.client.Exists(ctx, repoKey).Result()
    if err != nil {
        return err
    }

    if exists == 0 {
        // Cache miss - load from primary source and cache
        repo, err := c.loadRepositoryFromPrimary(ctx, repoID)
        if err != nil {
            return err
        }

        // Cache with appropriate TTL
        serialized, _ := json.Marshal(repo)
        err = c.client.SetEX(ctx, repoKey, serialized, c.ttl.RepositoryRouting).Err()
        if err != nil {
            log.Printf("Cache warming failed: %v", err)
            // Don't fail the operation if caching fails
        }
    }

    return nil
}
```

## Service Mesh and Container Orchestration

**Location**: `deploy/docker-compose.dev.yml`

### Network Topology
```yaml
networks:
  repo-context-network:
    driver: bridge
    ipam:
      config:
        - subnet: 172.20.0.0/16
```

### Service Dependencies and Health Checks
```yaml
apiserver:
  depends_on:
    - redis
    - weaviate
  healthcheck:
    test: ["CMD", "wget", "--tries=1", "--spider", "http://localhost:8080/health"]
    interval: 30s
    timeout: 10s
    retries: 5
    start_period: 40s
```

### Resource Limits and Scaling
```yaml
weaviate:
  deploy:
    resources:
      limits:
        memory: 4G
        cpus: '2.0'
      reservations:
        memory: 2G
        cpus: '1.0'
```

## Observability and Monitoring Architecture

### Distributed Tracing Flow
```
HTTP Request → nginx → apiserver → [gRPC Services] → [External APIs]
     │              │         │                            │
     └──────── Jaeger Span ────────── Child Spans ────────┘
                    │
            ┌───────▼────────┐
            │ Trace Context  │
            │ - Request ID   │
            │ - User ID      │
            │ - Repository   │
            │ - Timing       │
            └────────────────┘
```

### Metrics Collection Strategy
```go
// Metrics are recorded at key decision points throughout the request lifecycle
func (s *ChatServer) ChatWithRepository(stream repocontextv1.ChatService_ChatWithRepositoryServer) error {
    timer := observability.StartTimer()
    defer func() {
        s.metrics.RecordRPCLatency("ChatWithRepository", timer.Duration())
    }()

    // Record search backend performance
    searchTimer := observability.StartTimer()
    results, err := s.performDualSearch(ctx, repositoryID, query, limit)
    s.metrics.RecordBackendLatency("dual_search", searchTimer.Duration())
    s.metrics.RecordSearchResults("total", len(results))

    // Record LLM composition metrics
    compositionTimer := observability.StartTimer()
    response, err := s.composer.ComposeAnswerStream(ctx, query, results, tokenCallback)
    s.metrics.RecordBackendLatency("llm_composition", compositionTimer.Duration())
    s.metrics.RecordTokensGenerated(len(response.FullResponse))

    return nil
}
```

## Security Considerations

### Current Development Security Model
- **No authentication** (local development only)
- **Docker network isolation** (services not exposed externally)
- **API key protection** (environment variables, not committed)
- **Input validation** (protobuf schema enforcement)

### Production Security Enhancements Needed
```go
// Example production security middleware (not implemented)
func authenticationInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
    // Extract and validate JWT token
    token, err := extractBearerToken(ctx)
    if err != nil {
        return nil, status.Errorf(codes.Unauthenticated, "missing or invalid token")
    }

    // Validate token and extract claims
    claims, err := validateJWT(token)
    if err != nil {
        return nil, status.Errorf(codes.Unauthenticated, "invalid token")
    }

    // Add user context for authorization
    ctx = context.WithValue(ctx, "user_id", claims.UserID)
    ctx = context.WithValue(ctx, "tenant_id", claims.TenantID)

    return handler(ctx, req)
}
```

## Performance Characteristics and Bottlenecks

### Request Flow Timing Analysis
```
User Query → WebSocket → gRPC → Dual Search → LLM Composition → Response
    5ms        3ms       50ms        2s            1s           Total: ~3s
```

### Identified Bottlenecks
1. **OpenAI API Latency**: 1-2s for embedding generation
2. **Weaviate Vector Search**: 100-500ms for large collections
3. **DeepSeek LLM**: 1-3s for response generation
4. **File I/O**: 10-100ms for large repository access

### Scaling Strategies
```go
// Example connection pooling for external APIs (concept)
type PooledAPIClient struct {
    pool    *sync.Pool
    clients []*http.Client
    semaphore chan struct{}  // Rate limiting
}

func (p *PooledAPIClient) GetClient() *http.Client {
    p.semaphore <- struct{}{}  // Acquire rate limit token

    client := p.pool.Get()
    if client == nil {
        return &http.Client{Timeout: 30 * time.Second}
    }
    return client.(*http.Client)
}

func (p *PooledAPIClient) ReturnClient(client *http.Client) {
    p.pool.Put(client)
    <-p.semaphore  // Release rate limit token
}
```


## 🔄 Control Flow Diagrams

### 1. **Repository Ingestion Pipeline**: HTTP → gRPC → Processing

```
HTTP Request: POST /v1/upload/git
    ↓
┌─────────────────────────────────────────────────────────────────┐
│ HTTP Server (cmd/apiserver/main.go:238)                        │
│ • gRPC-Gateway: runtime.NewServeMux()                          │
│ • Route: RegisterUploadServiceHandlerFromEndpoint()            │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ gRPC Call
┌─────────────────────────────────────────────────────────────────┐
│ gRPC UploadService (internal/api/upload.go:45)                 │
│ • Method: UploadGitRepository()                                 │
│ • Validation: tenant_id, idempotency_key, git_repository       │
│ • Cache Check: Redis idempotency lookup                        │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ Async Processing
┌─────────────────────────────────────────────────────────────────┐
│ Ingestion Pipeline (internal/ingest/inline.go:89)              │
│ • STATE_PENDING → STATE_EXTRACTING                             │
│   - ProcessGitRepository() → git clone --depth=1               │
│ • STATE_EXTRACTING → STATE_CHUNKING                            │
│   - chunkFiles() → Intelligent language-specific chunking     │
│ • STATE_CHUNKING → STATE_EMBEDDING                             │
│   - OpenAI text-embedding-ada-002 API (batch processing)      │
│ • STATE_EMBEDDING → STATE_INDEXING                             │
│   - Weaviate collection creation & vector storage             │
│ • STATE_INDEXING → STATE_READY                                 │
│   - Redis status update & cache prefill                       │
└─────────────────────────────────────────────────────────────────┘
```

**Function Call Chain:**
1. `cmd/apiserver/main.go:238` → `RegisterUploadServiceHandlerFromEndpoint()`
2. `internal/api/upload.go:45` → `UploadGitRepository()`
3. `internal/api/upload.go:78` → `cache.SetUploadStatus()`
4. `internal/api/upload.go:85` → `ingestProvider.ProcessRepository()` (async)
5. `internal/ingest/inline.go:89` → `ProcessGitRepository()`
6. `internal/ingest/inline.go:156` → `chunkFiles()`
7. `internal/ingest/inline.go:290` → `generateEmbeddings()`
8. `internal/ingest/inline.go:345` → `indexChunks()`

### 2. **Query Pipeline**: WebSocket → gRPC → Dual Search → LLM

```
WebSocket: ws://localhost:3000/v1/chat/{repo_id}/stream
    ↓
┌─────────────────────────────────────────────────────────────────┐
│ WebSocket Bridge (internal/api/websocket.go:89)                │
│ • Connection Management: Gorilla WebSocket                     │
│ • Protocol Translation: JSON ↔ Protobuf                       │
│ • Route: /v1/chat/{repository_id}/stream                       │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ gRPC Stream
┌─────────────────────────────────────────────────────────────────┐
│ gRPC ChatService (internal/api/chat.go:89)                     │
│ • Method: ChatWithRepository() - Bidirectional Streaming       │
│ • Session Management: concurrent goroutine per connection      │
│ • Message Types: ChatStart, ChatMessage, ChatCancel            │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ Parallel Search
┌─────────────────────────────────────────────────────────────────┐
│ Dual Search Engine (internal/api/chat.go:245)                  │
│                                                                 │
│ ┌─────────────────────┐    ┌─────────────────────────────────┐ │
│ │ Lexical Search      │    │ Semantic Search                 │ │
│ │ (ripgrep)           │    │ (Weaviate + OpenAI)            │ │
│ │                     │    │                                 │ │
│ │ • ripgrep -i regex  │    │ • generateQueryEmbedding()      │ │
│ │ • File filtering    │    │ • Weaviate nearText query       │ │
│ │ • Fuzzy matching    │    │ • Vector similarity search     │ │
│ │ • Context lines     │    │ • Cosine distance ranking      │ │
│ └─────────────────────┘    └─────────────────────────────────┘ │
│           ↓                             ↓                      │
│ ┌─────────────────────────────────────────────────────────────┐ │
│ │ Result Merger (internal/query/merge.go:45)                 │ │
│ │ • Combine lexical + semantic results                       │ │
│ │ • Score normalization & ranking                            │ │
│ │ • Deduplication by file path + line range                  │ │
│ │ • Early hit streaming (EARLY phase)                        │ │
│ └─────────────────────────────────────────────────────────────┘ │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ LLM Composition
┌─────────────────────────────────────────────────────────────────┐
│ DeepSeek LLM Composition (internal/composer/deepseek.go:78)    │
│ • Context Assembly: Top-ranked code chunks                     │
│ • Prompt Engineering: Query + code context + instructions      │
│ • Token Streaming: Real-time response generation               │
│ • Citation Generation: File paths + line numbers               │
└─────────────────┬───────────────────────────────────────────────┘
                  ↓ Response Stream
┌─────────────────────────────────────────────────────────────────┐
│ Streaming Response Pipeline                                     │
│ 1. search_started    → Query acknowledgment                     │
│ 2. search_hit        → Early search results (EARLY phase)      │
│ 3. composition_started → LLM context summary                    │
│ 4. composition_token   → Real-time token streaming              │
│ 5. composition_complete → Final response with citations         │
│ 6. complete           → Session cleanup                         │
└─────────────────────────────────────────────────────────────────┘
```

**Function Call Chain:**
1. `internal/api/websocket.go:89` → `handleWebSocketConnection()`
2. `internal/api/websocket.go:156` → `ChatWithRepository()` (gRPC stream)
3. `internal/api/chat.go:89` → `ChatWithRepository()`
4. `internal/api/chat.go:245` → `performDualSearch()` (parallel goroutines)
5. `internal/query/lexical_rg.go:55` → `SearchLexical()` (ripgrep)
6. `internal/query/semantic_weaviate.go:67` → `SearchSemantic()` (Weaviate)
7. `internal/query/merge.go:45` → `MergeResults()`
8. `internal/composer/deepseek.go:78` → `ComposeResponse()` (streaming)

### 3. **Key gRPC ↔ Implementation Mappings**

| **HTTP Route** | **gRPC Service.Method** | **Implementation File** | **Core Function** |
|---------------|------------------------|------------------------|------------------|
| `POST /v1/upload/git` | `UploadService.UploadGitRepository` | `internal/api/upload.go` | `UploadGitRepository()` |
| `GET /v1/upload/{id}/status` | `UploadService.GetUploadStatus` | `internal/api/upload.go` | `GetUploadStatus()` |
| `GET /v1/repositories` | `RepositoryService.ListRepositories` | `internal/api/repository.go` | `ListRepositories()` |
| `GET /v1/repositories/{id}` | `RepositoryService.GetRepository` | `internal/api/repository.go` | `GetRepository()` |
| `DELETE /v1/repositories/{id}` | `RepositoryService.DeleteRepository` | `internal/api/repository.go` | `DeleteRepository()` |
| `GET /health` | `HealthService.Check` | `internal/api/health.go` | `Check()` |
| `ws://.../v1/chat/{id}/stream` | `ChatService.ChatWithRepository` | `internal/api/chat.go` + `websocket.go` | `ChatWithRepository()` |

This architecture enables high-performance, concurrent repository analysis with multiple protocol support while maintaining type safety and observability throughout the system.

