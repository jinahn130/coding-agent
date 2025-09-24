# Claude Context File

This file provides comprehensive context for Claude to understand and work with the Repository Context + Coding Agent Service codebase.

## System Overview

This is a **gRPC-first service** that provides intelligent repository Q&A through a simple upload-once workflow. Users upload repositories (via Git URL or file) and can ask questions about the code using a combination of lexical and semantic search with LLM composition.

### Key Design Principles

1. **Simple Q&A Focus**: Not a version-tracking coding agent - users upload repos once and ask questions
2. **Upload-Once Workflow**: No commit history tracking or incremental updates
3. **Idempotency**: Same repository + same idempotency key = same result (no duplicate indexing)
4. **Local-First**: All services run locally via Docker, no cloud dependencies required
5. **Dual Search**: Combines ripgrep (lexical) + Weaviate/OpenAI embeddings (semantic)

## Architecture Components

### Core Services
- **API Server** (Go): Main gRPC server with HTTP gateway
- **Web UI** (served via Nginx): Simple upload and chat interface
- **Weaviate**: Local vector database for semantic search
- **Redis**: Caching layer for metadata and search results

### External APIs
- **OpenAI**: text-embedding-ada-002 for code embeddings
- **DeepSeek**: LLM for chat responses and code explanations

### Observability Stack
- **Prometheus**: Metrics collection
- **Jaeger**: Distributed tracing (currently stub implementation)
- **Grafana**: Monitoring dashboards

## Key File Structure

```
repo-context-service/
├── cmd/apiserver/           # Main application entry point
├── internal/
│   ├── api/                 # gRPC service implementations
│   │   ├── upload.go        # Repository upload handling
│   │   ├── chat.go          # Q&A streaming service
│   │   └── repository.go    # Repository management
│   ├── ingest/             # Repository processing pipeline
│   │   └── inline.go       # Git clone, chunking, embedding
│   ├── query/              # Search backends
│   │   ├── semantic_weaviate.go  # Weaviate integration
│   │   └── lexical_ripgrep.go    # ripgrep integration
│   ├── cache/              # Redis caching layer
│   ├── config/             # Configuration management
│   └── observability/      # Metrics and tracing (stub)
├── proto/                  # Protocol buffer definitions
├── deploy/                 # Docker and deployment configs
└── ui/web/                 # Static web UI files (served by container)
```

## Important Implementation Details

### Repository Upload Flow

1. **HTTP Endpoint**: `POST /v1/upload/git` (newly added for UI compatibility)
2. **Idempotency Check**: Redis cache lookup by `tenant_id + idempotency_key`
3. **Git Clone**: Uses standard `git clone --depth=1` in Docker container
4. **Processing Pipeline**: EXTRACTING → CHUNKING → EMBEDDING → INDEXING → READY
5. **Storage**: Weaviate collection per repository (repo ID as collection name)

### Search and Chat Flow

1. **Dual Search**: Parallel lexical (ripgrep) + semantic (Weaviate) search
2. **Result Merging**: Combines and ranks results by relevance score
3. **Streaming Response**: Early search hits → LLM composition → citations
4. **Caching**: Redis caches search results and metadata

### Configuration Management

- **Environment Variables**: Required OPENAI_API_KEY and DEEPSEEK_API_KEY
- **Docker Compose**: All services defined in `deploy/docker-compose.dev.yml`
- **Makefile**: Build automation with `make up` requiring API keys as env vars

## Recent Key Changes (2024-09-24)

### Major Implementation: Complete Chat System

**WebSocket Bridge Implementation**
- **New File**: `internal/api/websocket.go` - Complete WebSocket to gRPC bridge
- Gorilla WebSocket and Mux dependencies added for real-time chat
- Bidirectional streaming: WebSocket JSON ↔ gRPC Protobuf conversion
- Connection management with concurrent session handling

**Complete Search Integration**
- **Updated**: `internal/api/chat.go` - Replaced all TODO comments with working implementations
- `performDualSearch()` method: Parallel lexical (ripgrep) + semantic (Weaviate) search
- Result merging and ranking with early search hit streaming
- Real-time response composition with DeepSeek LLM

**Query Embedding Integration**
- `generateQueryEmbedding()` method using OpenAI text-embedding-ada-002
- ChatServer constructor updated to accept embedding client dependency
- Main.go updated with proper dependency injection throughout

**UI and API Fixes**
- Fixed protobuf field naming issues (`Id` vs `RepositoryId`)
- Updated JavaScript WebSocket handling with proper field names
- Cache busting for frontend updates (`app.js?v=20250924-chat`)
- Enabled real chat functionality after backend completion

**Developer Tools**
- **New File**: `scripts/cleanup.sh` - Comprehensive development data cleanup
- Reliable Redis, Weaviate, and repository file cleanup for development workflow

### Current Working State

- ✅ Complete end-to-end chat system functional
- ✅ WebSocket streaming with dual search (lexical + semantic)
- ✅ OpenAI embedding generation for query processing
- ✅ DeepSeek LLM response composition with streaming
- ✅ Real-time UI with streaming chat messages
- ✅ Repository upload and processing pipeline
- ✅ Development cleanup tools and debugging commands

## API Endpoints Reference

### Working Endpoints

| Method | Endpoint | Purpose | Status |
|--------|----------|---------|--------|
| `POST` | `/v1/upload/git` | Upload Git repository | ✅ Working |
| `GET` | `/v1/upload/{id}/status` | Check processing status | ✅ Working |
| `GET` | `/v1/repositories` | List repositories | ✅ Working |
| `GET` | `/v1/chat/{repository_id}/stream` | WebSocket chat stream | ✅ Working |
| `GET` | `/health` | Health check | ✅ Working |

### Chat System Architecture

**WebSocket Message Flow:**
1. **Start**: `{"start": {"repository_id": "...", "tenant_id": "local", "options": {...}}}`
2. **Chat Message**: `{"chat_message": {"query": "...", "session_id": "..."}}`
3. **Response Stream**:
   - `{"search_started": {"session_id": "...", "query_id": "..."}}`
   - `{"search_hit": {"phase": "EARLY", "rank": 1, "chunk": {...}}}`
   - `{"composition_started": {"context_chunks": 5}}`
   - `{"composition_token": {"text": "..."}}`
   - `{"composition_complete": {"full_response": "..."}}`
   - `{"complete": {"session_id": "...", "query_id": "..."}}`

**Dual Search Implementation:**
- **Lexical Search**: ripgrep with regex support for exact code matches
- **Semantic Search**: Weaviate with OpenAI embeddings for conceptual matches
- **Result Merging**: Combined ranking based on relevance scores
- **Early Streaming**: Search hits sent immediately while LLM processes context

### Service Ports

- **API Server**: 8080 (HTTP), 9090 (gRPC), 8081 (admin/metrics)
- **Web UI**: 3000
- **Weaviate**: 8082
- **Redis**: 6379
- **Grafana**: 3001 (admin/admin)
- **Jaeger**: 16686
- **Prometheus**: 9091

## Development Workflow

### First-Time Setup
```bash
make setup     # Install tools, download deps, generate proto files
make up        # Start all services (requires API keys in env)
```

### Common Development Tasks
```bash
# View logs
docker-compose -f deploy/docker-compose.dev.yml logs -f apiserver

# ⚠️ IMPORTANT: If code changes don't appear, force rebuild
make up-fresh    # Force rebuild all Docker images (5-10 min)

# Test upload
curl -X POST http://localhost:8080/v1/upload/git \
  -H "Content-Type: application/json" \
  -d '{"git_repository": {"url": "https://github.com/user/repo"}, "tenant_id": "local"}'

# Check status
curl "http://localhost:8080/v1/upload/{upload-id}/status?tenant_id=local"

# Test WebSocket chat (with wscat)
wscat -c ws://localhost:3000/v1/chat/{repository-id}/stream

# Clean development data
./scripts/cleanup.sh

# Monitor Weaviate collections
curl http://localhost:8082/v1/schema

# Check Redis cache
docker exec repo-context-redis redis-cli keys "*"
```

## Troubleshooting Guide

### 🚨 **Critical: Docker Cache Issues**

**Problem**: Code changes don't appear when testing (most common development issue).

**Symptoms**:
- Debug logs you added aren't showing up
- Bug fixes don't take effect
- New features don't work despite correct implementation
- Behavior doesn't match code changes

**Root Cause**: Docker is using cached layers with old code.

**Solution**: Force rebuild all images
```bash
make up-fresh    # Takes 5-10 minutes but guarantees fresh code
```

**When to use `make up-fresh`:**
- After significant code changes
- When debug logs don't appear
- Before important demos or testing
- After git pulls with major changes

### Environment Issues

**"OPENAI_API_KEY is required"**
- Ensure `export OPENAI_API_KEY="sk-..."` before `make up`
- Makefile validates env vars are set

**"make up" fails**
- Run from repo root directory (not parent)
- Check Docker is running
- Verify API keys are exported

### Repository Processing Issues

**Stuck in CHUNKING state**
- Check OpenAI API quota/rate limits
- View apiserver logs for specific errors
- Embedding generation is the most common bottleneck

**Git clone fails**
- Ensure repository is publicly accessible
- Test manual clone: `docker exec repo-context-apiserver git clone <url> /tmp/test`
- Check network connectivity from container

### Service Health

**UI shows empty repositories**
- Wait for STATE_READY status
- Check Weaviate: `curl http://localhost:8082/v1/.well-known/ready`
- Verify Redis is working: `docker-compose logs redis`

### Chat System Issues

**WebSocket connection fails**
- Ensure repository is in STATE_READY status
- Check apiserver logs: `docker-compose logs -f apiserver`
- Verify gRPC server is running on port 9090
- Test WebSocket endpoint accessibility

**Chat returns empty responses**
- Verify repository has been fully indexed (check Weaviate collections)
- Check OpenAI API key is valid and has sufficient quota
- Ensure DeepSeek API key is configured correctly
- Try different query phrasing or keywords

**Search results don't appear**
- Check repository files were properly chunked during ingestion
- Verify both lexical (ripgrep) and semantic (Weaviate) backends are working
- Monitor search timing in apiserver logs
- Test individual search backends separately

## Code Patterns and Conventions

### Error Handling
- Use gRPC status codes: `status.Errorf(codes.InvalidArgument, "message")`
- Wrap errors with context: `fmt.Errorf("operation failed: %w", err)`

### Observability
- All RPC methods use `tracer.StartRPC()` (currently no-op stub)
- Metrics recorded via `metrics.RecordXXX()` methods
- Structured logging with context

### Database/Cache
- Redis keys: `tenant:{tenant_id}:upload:{upload_id}`
- Weaviate collections: One per repository (repo ID as collection name)
- Idempotency via Redis cache lookup

## Testing Approach

### Manual Testing
1. Upload repository via API
2. Monitor status progression
3. Test search/chat functionality (when ready)

### Integration Testing
- Docker-based: All services must be running
- API-first: Test HTTP endpoints directly
- State verification: Check Redis cache and Weaviate collections

## Dependencies and Versions

### Go Modules
- Go 1.23
- gRPC ecosystem: grpc, grpc-gateway, protobuf
- OpenAI client: sashabaranov/go-openai
- Weaviate client: weaviate-go-client/v4
- Redis: go-redis/redis
- WebSocket: gorilla/websocket
- HTTP routing: gorilla/mux
- No direct OpenTelemetry deps (to avoid conflicts)

### Docker Images
- golang:1.23-alpine (build)
- alpine:3.18 (runtime)
- weaviate/weaviate (vector DB)
- redis:7-alpine (caching)

## Security Considerations

### Current State (Development)
- No authentication required
- Local-only deployment
- API keys in environment variables

### Production Considerations
- Add authentication middleware
- Use managed Redis/Weaviate
- Secure API key management
- Rate limiting and CORS

## Performance Characteristics

### Expected Timings (48GB RAM)
- Small repos (1K files): 2-5 minutes
- Medium repos (5K files): 5-10 minutes
- Large repos (15K+ files): 10-20 minutes

### Bottlenecks
1. **OpenAI API**: Embedding generation (rate limited)
2. **Git Clone**: Network bandwidth for large repos
3. **Weaviate Indexing**: CPU/memory intensive for large collections

### Optimization Opportunities
- Parallel embedding generation (batch API calls)
- Smart chunking (avoid duplicate content)
- Caching at multiple levels

## Future Enhancement Ideas

### Incremental Updates
- Track commit SHAs for smart re-indexing
- Delta processing for changed files only

### Multi-Language Support
- Language-specific chunking strategies
- Improved symbol detection and extraction

### Advanced Search
- Hybrid search with learned ranking
- Code structure aware search (AST-based)

### Chat Enhancements
- Redis caching for query results (planned for future implementation)
- Multi-turn conversation context tracking
- Code citation improvements with syntax highlighting
- Advanced streaming with progress indicators

## Implementation Details

### WebSocket to gRPC Bridge Architecture

The chat system uses a sophisticated WebSocket to gRPC bridge that handles:

1. **Connection Management**: Concurrent WebSocket connections with unique connection IDs
2. **Protocol Translation**: JSON WebSocket messages ↔ Protobuf gRPC messages
3. **Streaming Coordination**: Bidirectional streaming with proper goroutine coordination
4. **Error Handling**: Connection failures, timeouts, and graceful disconnection

**Key Files:**
- `internal/api/websocket.go`: Complete bridge implementation
- `internal/api/chat.go`: gRPC chat service with dual search
- `cmd/apiserver/main.go`: Gorilla Mux integration with HTTP routing

### Dual Search Implementation

The search system combines two complementary approaches:

1. **Lexical Search (ripgrep)**:
   - Exact string and regex matching
   - Fast file system scanning
   - Perfect for finding specific functions, variables, error messages

2. **Semantic Search (Weaviate + OpenAI)**:
   - Conceptual similarity matching
   - Vector embeddings using text-embedding-ada-002
   - Excellent for finding related code patterns, similar functionality

3. **Result Merging**:
   - Combines results from both backends
   - Ranks by relevance score and search type
   - Streams early hits while LLM processes context

### Streaming Response Architecture

Chat responses follow a multi-phase streaming protocol:

1. **Search Started**: Confirms query received and search initiated
2. **Search Hits**: Early results streamed as they arrive (EARLY phase)
3. **Composition Started**: LLM processing begins with context chunk count
4. **Composition Tokens**: Real-time streaming of LLM response tokens
5. **Composition Complete**: Final response with complete text
6. **Complete**: Session cleanup and completion confirmation

This provides immediate feedback to users while maintaining real-time response streaming.

This context should provide comprehensive understanding for future development, debugging, and enhancement of the Repository Context Service.