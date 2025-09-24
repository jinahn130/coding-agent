# Repository Context + Coding Agent Service

A gRPC-first service that provides intelligent repository Q&A through a simple upload-once workflow. Upload any repository and get an AI-powered chatbot that understands your code using dual search backends (lexical + semantic) and LLM composition.

## Features

🚀 **Simple Repository Q&A**: Upload repositories once and ask questions about your code
🔍 **Dual Search**: Combines lexical (ripgrep) and semantic (Weaviate + OpenAI embeddings) search
⚡ **Real-time Chat**: WebSocket-powered streaming responses with early search hits + progressive LLM composition
💬 **Interactive UI**: Full-featured web chat interface with repository selection and streaming responses
🏗️ **gRPC-First**: Full gRPC API with HTTP gateway, WebSocket bridge, and web UI
📦 **Multi-Repository**: Manage and chat with multiple uploaded repositories simultaneously
🐳 **Local Deployment**: Complete Docker Compose setup - no cloud dependencies required
🏠 **Self-Contained**: Local Weaviate instance for vector storage and Redis for caching
📊 **Built-in Monitoring**: Prometheus metrics, Jaeger tracing, Grafana dashboards

## Architecture

```
┌─────────────┐ HTTP  ┌─────────────┐ gRPC ┌─────────────┐
│   Web UI    │──────▶│ HTTP Server │─────▶│ gRPC Server │
│             │ WS    │ (Gateway +  │      │    (Go)     │
│ Upload/Chat │──────▶│  WebSocket) │      │             │
└─────────────┘       └─────────────┘      └─────────────┘
                                                   │
                                   ┌───────────────┼───────────────┐
                                   │               │               │
                          ┌────────▼────┐ ┌───────▼────┐ ┌────────▼─────┐
                          │  Ingestion  │ │Dual Search │ │Chat Service  │
                          │  Pipeline   │ │   Engine   │ │(WebSocket)   │
                          └─────────────┘ └────────────┘ └──────────────┘
                                   │               │               │
                          ┌────────▼────┐ ┌───────▼────┐ ┌────────▼─────┐
                          │   OpenAI    │ │  Weaviate  │ │   DeepSeek   │
                          │ Embeddings  │ │   Vector   │ │     LLM      │
                          │     API     │ │     DB     │ │              │
                          └─────────────┘ └────────────┘ └──────────────┘

                                         ┌─────────────┐ ┌──────────────┐
                                         │    Redis    │ │   ripgrep    │
                                         │    Cache    │ │   (Lexical   │
                                         │             │ │   Search)    │
                                         └─────────────┘ └──────────────┘
```

## Quick Start

### Prerequisites

- Docker and Docker Compose
- OpenAI API key (for embeddings)
- DeepSeek API key (for chat responses)

### 1. Clone and Configure

```bash
git clone <repository-url>
cd repo-context-service

# Copy and configure environment
cp .env.example .env
# Edit .env with your API keys
```

### 2. Set Required Environment Variables

```bash
export OPENAI_API_KEY="your-openai-api-key-here"
export DEEPSEEK_API_KEY="your-deepseek-api-key-here"
```

### 3. Start Services

```bash
# Start all services with Docker Compose
make up
```

This will start:
- **API Server**: localhost:8080 (HTTP), localhost:9090 (gRPC)
- **Web UI**: http://localhost:3000
- **Weaviate**: localhost:8082 (local vector database)
- **Redis**: localhost:6379 (caching layer)
- **Grafana**: http://localhost:3001 (admin/admin)
- **Jaeger**: http://localhost:16686 (tracing)
- **Prometheus**: http://localhost:9091 (metrics)

### 4. Verify Deployment

```bash
# Check API health
curl http://localhost:8080/health

# Check if all services are running
docker ps | grep repo-context

# Open web UI
open http://localhost:3000
```

## Usage

### Web UI (Recommended)

1. **Access UI**: Go to http://localhost:3000

2. **Upload Repository**:
   - **Git URL**: Enter any public GitHub repository URL (e.g., `https://github.com/user/repo`)
   - **File Upload**: Drag and drop ZIP/tar files

3. **Monitor Processing**:
   - Watch progress: PENDING → EXTRACTING → CHUNKING → EMBEDDING → INDEXING → READY
   - Processing time: ~5-15 minutes depending on repository size

4. **Chat with Your Code**:
   - **Select Repository**: Click on a repository in READY state to enable chat
   - **Real-time Streaming**: Type questions and get streaming responses with code context
   - **Example Questions**:
     - "How does authentication work in this codebase?"
     - "Where are the API endpoints defined?"
     - "Show me the database schema and models"
     - "Find all error handling patterns"
     - "Explain the main application architecture"
   - **Advanced Features**:
     - Early search hits shown while LLM composes response
     - Code citations with file paths and line numbers
     - Dual search combines exact matches + semantic understanding
     - WebSocket streaming for real-time interaction

### API Examples

#### Upload a Git Repository

```bash
curl -X POST http://localhost:8080/v1/upload/git \
  -H "Content-Type: application/json" \
  -d '{
    "git_repository": {
      "url": "https://github.com/user/repo.git",
      "ref": "main"
    },
    "tenant_id": "local",
    "idempotency_key": "my-repo-upload-1"
  }'

# Response:
# {
#   "uploadId": "my-repo-upload-1",
#   "repositoryId": "repo-1234567890",
#   "acceptedAt": "2024-01-01T10:00:00Z",
#   "status": {"state": "STATE_PENDING"}
# }
```

#### Check Processing Status

```bash
curl "http://localhost:8080/v1/upload/my-repo-upload-1/status?tenant_id=local"

# Response shows current state:
# - STATE_PENDING: Upload accepted
# - STATE_EXTRACTING: Cloning repository
# - STATE_CHUNKING: Breaking code into chunks
# - STATE_EMBEDDING: Generating embeddings
# - STATE_INDEXING: Storing in Weaviate
# - STATE_READY: Ready for questions
# - STATE_FAILED: Something went wrong
```

#### List Repositories

```bash
curl "http://localhost:8080/v1/repositories?tenant_id=local"
```

#### Chat via WebSocket

The chat functionality uses WebSocket for real-time streaming responses:

```javascript
// Connect to chat WebSocket
const ws = new WebSocket('ws://localhost:3000/v1/chat/{repository_id}/stream');

// Start chat session
ws.send(JSON.stringify({
  start: {
    repository_id: "repo-1234567890",
    tenant_id: "local",
    options: {
      max_results: 10,
      stream_tokens: true
    }
  }
}));

// Send chat message
ws.send(JSON.stringify({
  chat_message: {
    query: "How does authentication work?",
    session_id: "session-123"
  }
}));

// Handle streaming responses
ws.onmessage = (event) => {
  const data = JSON.parse(event.data);

  if (data.search_started) {
    console.log('Search started:', data.search_started.session_id);
  }

  if (data.search_hit) {
    console.log('Search hit:', data.search_hit.chunk.file_path);
  }

  if (data.composition_token) {
    console.log('LLM token:', data.composition_token.text);
  }

  if (data.composition_complete) {
    console.log('Final response:', data.composition_complete.full_response);
  }
};
```

### Repository Size Limits

With 48GB RAM, you can comfortably process:

| Repository Type | Compressed Size | Files | Memory Usage |
|----------------|-----------------|-------|--------------|
| **Small Projects** | Up to 500MB | 1K-5K files | 2-4GB |
| **Large Projects** | 500MB-2GB | 5K-15K files | 4-8GB |
| **Very Large** | 2GB-8GB | 15K+ files | 8-16GB |

**Optimal range**: 1-4GB compressed repositories with 10K-30K files.

## Development

### First-Time Setup

**⚠️ IMPORTANT**: Run this before building:

```bash
# Install tools, dependencies, and generate proto files
make setup

# Then build and run
make build
make run
```

### Available Commands

```bash
make setup         # First-time setup (required)
make build         # Build the binary
make run           # Build and run locally
make up            # Start all services with Docker
make down          # Stop all services
make test          # Run tests
make lint          # Run linters
make fmt           # Format code

# Data Management
./scripts/cleanup.sh # Clean all uploaded repositories, Redis, and Weaviate data
```

### Development Workflow

```bash
# Start services for development
make up

# ⚠️ If you make code changes and they don't appear:
make up-fresh    # Force rebuild with no cache (takes 5-10 min)

# Quick restart (if changes are in Go code only, not Docker config)
make build
docker-compose -f deploy/docker-compose.dev.yml restart apiserver

# View logs
docker-compose -f deploy/docker-compose.dev.yml logs -f apiserver
```

## Configuration

### Environment Variables

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `OPENAI_API_KEY` | OpenAI API key for embeddings | ✅ | - |
| `DEEPSEEK_API_KEY` | DeepSeek API key for chat | ✅ | - |
| `TRACING_ENABLED` | Enable OpenTelemetry tracing | - | `true` |
| `UPLOAD_MAX_FILE_SIZE` | Max upload size in bytes | - | 100MB |
| `DEFAULT_CHUNK_SIZE` | Code chunk size in lines | - | 100 |

### Upload Configuration

- **Supported formats**: .zip, .tar, .tar.gz, .tgz, Git URLs
- **Auto-excluded**: node_modules/, vendor/, .git/, binaries, images, build artifacts
- **Languages supported**: Go, JavaScript, Python, Java, C/C++, Rust, and 20+ more

### Search Configuration

- **Lexical search**: ripgrep with regex support
- **Semantic search**: OpenAI text-embedding-ada-002 via Weaviate
- **Chunk strategy**: 100 lines with 10-line overlap for context
- **Result merging**: Combines and ranks lexical + semantic results

## Monitoring & Observability

### Metrics (Prometheus)

Access at http://localhost:9091:

- `rpc_requests_total` - API request counts by method and status
- `ingestion_duration_seconds` - Repository processing time
- `backend_latency_seconds` - Search performance by backend
- `cache_hits_total` - Redis cache effectiveness

### Tracing (Jaeger)

Access at http://localhost:16686 to view:
- End-to-end request traces
- Search backend performance
- LLM composition timing
- Error propagation

### Dashboards (Grafana)

Access at http://localhost:3001 (admin/admin):
- System overview dashboard
- Search performance metrics
- Repository ingestion stats

## Troubleshooting

### 🚨 **Most Common Issue: Code Changes Not Appearing**

**Problem**: You make code changes but they don't appear when testing (old behavior persists).

**Cause**: Docker is using cached images with old code.

**Solution**: Use the force rebuild command:
```bash
# 🔄 Force rebuild all Docker images from scratch
make up-fresh
```

**When to use `make up-fresh`:**
- After making code changes that don't appear in testing
- When debug logs or new functionality isn't working
- When you see unexpected behavior that doesn't match your code
- After git pulls with significant changes

**Note**: `make up-fresh` takes 5-10 minutes but ensures you're running the latest code.

### Other Common Issues

**"OPENAI_API_KEY is required" error:**
```bash
# Make sure environment variables are set
export OPENAI_API_KEY="sk-..."
export DEEPSEEK_API_KEY="sk-..."
make up
```

**Repository stuck in "CHUNKING" state:**
- Check API key quota: OpenAI embeddings API limits
- View logs: `docker-compose -f deploy/docker-compose.dev.yml logs -f apiserver`

**"Git clone failed: Upload failed: Not Found":**
- Use the new `/v1/upload/git` endpoint (not the old streaming endpoint)
- Ensure repository URL is publicly accessible
- Check network connectivity from Docker container

**UI shows empty repository list:**
- Wait for repository processing to complete (STATE_READY)
- Check upload status via API
- Verify Weaviate is running: `curl http://localhost:8082/v1/.well-known/ready`

**Chat interface doesn't enable after repository upload:**
- Ensure repository status is STATE_READY (not PENDING/PROCESSING)
- Check browser console for JavaScript errors
- Clear browser cache and reload the page
- Verify WebSocket connection: `curl -I -H "Upgrade: websocket" -H "Connection: upgrade" ws://localhost:3000/v1/chat/repo-123/stream`

**WebSocket chat connection fails:**
- Check that the apiserver container is running and healthy
- Verify repository ID exists: `curl "http://localhost:8080/v1/repositories?tenant_id=local"`
- Test WebSocket endpoint is accessible
- Check nginx is properly proxying WebSocket connections

**Chat returns empty or irrelevant responses:**
- Verify repository has completed embedding generation (STATE_READY)
- Check OpenAI API key is valid and has quota
- Try different query phrasing or keywords
- Verify Weaviate collections exist: `curl http://localhost:8082/v1/schema`

### Debug Commands

```bash
# Check all services
docker ps | grep repo-context

# View logs
docker-compose -f deploy/docker-compose.dev.yml logs -f apiserver

# Test git clone manually
docker exec repo-context-apiserver git clone https://github.com/user/repo /tmp/test

# Check Weaviate health and collections
curl http://localhost:8082/v1/.well-known/ready
curl http://localhost:8082/v1/schema

# Test API directly
curl -X POST http://localhost:8080/v1/upload/git \
  -H "Content-Type: application/json" \
  -d '{"git_repository": {"url": "https://github.com/microsoft/vscode"}, "tenant_id": "local"}'

# Test WebSocket chat connection
curl -I -H "Upgrade: websocket" -H "Connection: upgrade" \
  ws://localhost:3000/v1/chat/repo-123/stream

# Check Redis cache
docker exec repo-context-redis redis-cli keys "*"
docker exec repo-context-redis redis-cli get "upload:status:my-upload-id"

# Clean development data
./scripts/cleanup.sh
```

## System Requirements

### Minimum
- **RAM**: 8GB (for small repositories)
- **Storage**: 10GB free space
- **CPU**: 2+ cores
- **Network**: Internet access for API calls

### Recommended (48GB RAM)
- **Concurrent repositories**: 5-10
- **Repository size**: Up to 4GB compressed
- **Processing time**: 5-15 minutes per repository
- **Query response**: 1-3 seconds

## API Reference

### HTTP Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/v1/upload/git` | Upload Git repository |
| `GET` | `/v1/upload/{id}/status` | Check upload status |
| `GET` | `/v1/repositories` | List repositories |
| `GET` | `/v1/repositories/{id}` | Get repository details |
| `DELETE` | `/v1/repositories/{id}` | Delete repository |
| `GET` | `/health` | Health check |

### gRPC Services

- `UploadService` - Repository uploads
- `RepositoryService` - Repository management
- `ChatService` - Bidirectional streaming chat with dual search and LLM composition
- `HealthService` - Health checks

Use `grpcurl` to explore:
```bash
grpcurl -plaintext localhost:9090 list
grpcurl -plaintext localhost:9090 describe repocontext.v1.UploadService
grpcurl -plaintext localhost:9090 describe repocontext.v1.ChatService
```

### WebSocket Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/chat/{repository_id}/stream` | WebSocket chat stream with repository |

**WebSocket Message Protocol:**
- **Start**: Initialize chat session with repository
- **Chat Message**: Send query and receive streaming response
- **Cancel**: Cancel ongoing chat session

## Deployment Notes

### Docker Images
- **Base**: Alpine Linux with Git, ripgrep, and Go runtime
- **Size**: ~100MB compressed
- **Security**: Runs as non-root user

### Data Persistence
- **Weaviate**: Stores vector embeddings (persistent volume)
- **Redis**: Caches metadata and search results (ephemeral)
- **Repositories**: Stored in container (ephemeral in dev)

### Production Considerations
- Use external Redis for caching
- Consider managed Weaviate for scaling
- Enable authentication and rate limiting
- Set up proper backup strategies
- Monitor API key usage and costs

## License

[Add your license here]

## Contributing

1. Fork the repository
2. Run `make setup` for first-time development setup
3. Make changes and add tests
4. Run `make lint && make test`
5. Submit a pull request

## Support

- **Issues**: GitHub Issues
- **Logs**: `docker-compose logs`
- **Metrics**: http://localhost:9091
- **Tracing**: http://localhost:16686