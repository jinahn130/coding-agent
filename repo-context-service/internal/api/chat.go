package api

import (
	"context"
	"fmt"
	"sync"
	"time"

	"repo-context-service/internal/cache"
	"repo-context-service/internal/composer"
	"repo-context-service/internal/config"
	"repo-context-service/internal/ingest"
	"repo-context-service/internal/observability"
	"repo-context-service/internal/query"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ChatServer struct {
	repocontextv1.UnimplementedChatServiceServer
	config          *config.Config
	cache           *cache.RedisCache
	queryService    *QueryService
	composer        *composer.DeepSeekClient
	embeddingClient ingest.EmbeddingClient
	metrics         *observability.Metrics
	tracer          *observability.Tracer
	sessions        map[string]*ChatSession
	sessionsMutex   sync.RWMutex
}

type ChatSession struct {
	ID           string
	RepositoryID string
	TenantID     string
	Options      *repocontextv1.ChatOptions
	CreatedAt    time.Time
	Active       bool
	CancelFunc   context.CancelFunc
}

func NewChatServer(
	cfg *config.Config,
	cache *cache.RedisCache,
	queryService *QueryService,
	composer *composer.DeepSeekClient,
	embeddingClient ingest.EmbeddingClient,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *ChatServer {
	return &ChatServer{
		config:          cfg,
		cache:           cache,
		queryService:    queryService,
		composer:        composer,
		embeddingClient: embeddingClient,
		metrics:         metrics,
		tracer:          tracer,
		sessions:        make(map[string]*ChatSession),
	}
}

func (s *ChatServer) ChatWithRepository(stream repocontextv1.ChatService_ChatWithRepositoryServer) error {
	ctx := stream.Context()
	ctx, span := s.tracer.StartRPC(ctx, "ChatWithRepository")
	defer span.End()

	var session *ChatSession
	defer func() {
		if session != nil {
			s.cleanupSession(session.ID)
		}
	}()

	// Main message processing loop
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		switch msg := req.Message.(type) {
		case *repocontextv1.ChatRequest_Start:
			// Initialize session
			session, err = s.handleChatStart(ctx, stream, msg.Start)
			if err != nil {
				return err
			}

		case *repocontextv1.ChatRequest_ChatMessage:
			// Handle chat message
			if session == nil {
				return status.Errorf(codes.FailedPrecondition, "session not initialized")
			}
			err = s.handleChatMessage(ctx, stream, session, msg.ChatMessage)
			if err != nil {
				return err
			}

		case *repocontextv1.ChatRequest_Cancel:
			// Handle cancellation
			if session != nil && session.CancelFunc != nil {
				session.CancelFunc()
			}
			return nil

		default:
			return status.Errorf(codes.InvalidArgument, "unknown message type")
		}
	}
}

func (s *ChatServer) handleChatStart(ctx context.Context, stream repocontextv1.ChatService_ChatWithRepositoryServer, start *repocontextv1.ChatStart) (*ChatSession, error) {
	tenantID := start.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	// Validate repository exists and is ready
	repo, err := s.cache.GetRepositoryMetadata(ctx, tenantID, start.RepositoryId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get repository: %v", err)
	}

	if repo == nil {
		return nil, status.Errorf(codes.NotFound, "repository not found")
	}

	if repo.IngestionStatus.State != repocontextv1.IngestionStatus_STATE_READY {
		return nil, status.Errorf(codes.FailedPrecondition, "repository is not ready (status: %s)", repo.IngestionStatus.State)
	}

	// Create session
	sessionID := generateSessionID()
	_, cancel := context.WithCancel(ctx)

	session := &ChatSession{
		ID:           sessionID,
		RepositoryID: start.RepositoryId,
		TenantID:     tenantID,
		Options:      start.Options,
		CreatedAt:    time.Now(),
		Active:       true,
		CancelFunc:   cancel,
	}

	// Store session
	s.sessionsMutex.Lock()
	s.sessions[sessionID] = session
	s.sessionsMutex.Unlock()

	// Create a no-op span for tracing
	span := &observability.Span{}
	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
		observability.RepositoryAttr(start.RepositoryId),
	)

	return session, nil
}

func (s *ChatServer) handleChatMessage(ctx context.Context, stream repocontextv1.ChatService_ChatWithRepositoryServer, session *ChatSession, message *repocontextv1.ChatMessage) error {
	queryID := generateQueryID()

	// Send search started event
	err := stream.Send(&repocontextv1.ChatResponse{
		Message: &repocontextv1.ChatResponse_SearchStarted{
			SearchStarted: &repocontextv1.SearchStarted{
				SessionId: session.ID,
				QueryId:   queryID,
			},
		},
	})
	if err != nil {
		return err
	}

	// Record start time for metrics
	timer := observability.StartTimer()

	// Perform dual search (lexical + semantic)
	searchResults, err := s.performDualSearch(ctx, session.RepositoryID, message.Query, getTopK(session.Options))
	if err != nil {
		return status.Errorf(codes.Internal, "search failed: %v", err)
	}

	// Send early hits after getting first few results
	earlyHitsSent := false
	if len(searchResults) >= 3 {
		for i, result := range searchResults[:3] {
			err := stream.Send(&repocontextv1.ChatResponse{
				Message: &repocontextv1.ChatResponse_SearchHit{
					SearchHit: &repocontextv1.SearchHit{
						SessionId: session.ID,
						QueryId:   queryID,
						Phase:     repocontextv1.HitPhase_HIT_PHASE_EARLY,
						Rank:      int32(i + 1),
						Chunk:     result,
					},
				},
			})
			if err != nil {
				return err
			}
		}
		earlyHitsSent = true
		s.metrics.RecordTimeToFirstHit(timer.Duration())
	}

	// Send remaining results as final hits
	startIdx := 0
	if earlyHitsSent {
		startIdx = 3
	}

	for i := startIdx; i < len(searchResults); i++ {
		err := stream.Send(&repocontextv1.ChatResponse{
			Message: &repocontextv1.ChatResponse_SearchHit{
				SearchHit: &repocontextv1.SearchHit{
					SessionId: session.ID,
					QueryId:   queryID,
					Phase:     repocontextv1.HitPhase_HIT_PHASE_FINAL,
					Rank:      int32(i + 1),
					Chunk:     searchResults[i],
				},
			},
		})
		if err != nil {
			return err
		}
	}

	// Start composition phase
	err = stream.Send(&repocontextv1.ChatResponse{
		Message: &repocontextv1.ChatResponse_CompositionStarted{
			CompositionStarted: &repocontextv1.CompositionStarted{
				SessionId:     session.ID,
				QueryId:       queryID,
				ContextChunks: int32(len(searchResults)),
			},
		},
	})
	if err != nil {
		return err
	}

	compositionTimer := observability.StartTimer()

	// Compose answer using LLM
	if session.Options != nil && session.Options.StreamTokens {
		// Streaming composition
		result, err := s.composer.ComposeAnswerStream(ctx, message.Query, searchResults, func(token string) error {
			return stream.Send(&repocontextv1.ChatResponse{
				Message: &repocontextv1.ChatResponse_CompositionToken{
					CompositionToken: &repocontextv1.CompositionToken{
						SessionId: session.ID,
						QueryId:   queryID,
						Text:      token,
					},
				},
			})
		})
		if err != nil {
			return status.Errorf(codes.Internal, "composition failed: %v", err)
		}

		// Send final composition
		err = stream.Send(&repocontextv1.ChatResponse{
			Message: &repocontextv1.ChatResponse_CompositionComplete{
				CompositionComplete: &repocontextv1.CompositionComplete{
					SessionId:    session.ID,
					QueryId:      queryID,
					FullResponse: result.FullResponse,
					Citations:    result.Citations,
				},
			},
		})
		if err != nil {
			return err
		}

	} else {
		// Non-streaming composition
		result, err := s.composer.ComposeAnswer(ctx, message.Query, searchResults)
		if err != nil {
			return status.Errorf(codes.Internal, "composition failed: %v", err)
		}

		err = stream.Send(&repocontextv1.ChatResponse{
			Message: &repocontextv1.ChatResponse_CompositionComplete{
				CompositionComplete: &repocontextv1.CompositionComplete{
					SessionId:    session.ID,
					QueryId:      queryID,
					FullResponse: result.FullResponse,
					Citations:    result.Citations,
				},
			},
		})
		if err != nil {
			return err
		}
	}

	s.metrics.RecordTimeToSummary(compositionTimer.Duration())

	// Send completion message
	err = stream.Send(&repocontextv1.ChatResponse{
		Message: &repocontextv1.ChatResponse_Complete{
			Complete: &repocontextv1.ChatComplete{
				SessionId: session.ID,
				QueryId:   queryID,
				Timings: &repocontextv1.SearchTimings{
					// These would be filled from actual search timings
					LexicalMs:     100, // Placeholder
					SemanticMs:    200, // Placeholder
					MergeMs:       10,  // Placeholder
					CompositionMs: int32(compositionTimer.Duration().Milliseconds()),
					CacheHit:      false,
				},
				Stats: &repocontextv1.SearchStats{
					LexicalCandidates:  int32(len(searchResults)),
					SemanticCandidates: int32(len(searchResults)),
					MergedResults:      int32(len(searchResults)),
					ResultsTruncated:   false,
				},
			},
		},
	})

	return err
}

func (s *ChatServer) cleanupSession(sessionID string) {
	s.sessionsMutex.Lock()
	defer s.sessionsMutex.Unlock()

	if session, exists := s.sessions[sessionID]; exists {
		if session.CancelFunc != nil {
			session.CancelFunc()
		}
		delete(s.sessions, sessionID)
	}
}

// Helper functions

func generateSessionID() string {
	return fmt.Sprintf("session-%d", time.Now().UnixNano())
}

func generateQueryID() string {
	return fmt.Sprintf("query-%d", time.Now().UnixNano())
}

func getTopK(options *repocontextv1.ChatOptions) int32 {
	if options != nil && options.MaxResults > 0 {
		return options.MaxResults
	}
	return 10 // Default
}

// performDualSearch performs both lexical and semantic search and merges results
func (s *ChatServer) performDualSearch(ctx context.Context, repositoryID, queryText string, limit int32) ([]*repocontextv1.CodeChunk, error) {
	// Perform lexical search using ripgrep
	lexicalResults, err := s.queryService.lexicalClient.SearchLexical(ctx, repositoryID, queryText, int(limit), nil)
	if err != nil {
		return nil, fmt.Errorf("lexical search failed: %w", err)
	}

	// Generate embedding for semantic search
	queryEmbedding, err := s.generateQueryEmbedding(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("failed to generate query embedding: %w", err)
	}

	// Perform semantic search using Weaviate
	semanticResults, err := s.queryService.semanticClient.SearchSemantic(ctx, repositoryID, queryEmbedding, int(limit), nil)
	if err != nil {
		return nil, fmt.Errorf("semantic search failed: %w", err)
	}

	// Merge and rank results
	mergedResults := s.queryService.merger.MergeAndRank(&query.SearchResults{
		LexicalChunks:  lexicalResults,
		SemanticChunks: semanticResults,
	})

	// Convert to final results (take top results based on limit)
	maxResults := int(limit)
	if len(mergedResults.Chunks) < maxResults {
		maxResults = len(mergedResults.Chunks)
	}

	return mergedResults.Chunks[:maxResults], nil
}

// generateQueryEmbedding generates an embedding for the search query
func (s *ChatServer) generateQueryEmbedding(ctx context.Context, queryText string) ([]float32, error) {
	// Use the embedding client to generate query embeddings
	embeddings, err := s.embeddingClient.GenerateEmbeddings(ctx, []string{queryText}, "text-embedding-ada-002")
	if err != nil {
		return nil, fmt.Errorf("failed to generate embedding: %w", err)
	}

	if len(embeddings) == 0 || len(embeddings[0]) == 0 {
		return nil, fmt.Errorf("received empty embedding")
	}

	return embeddings[0], nil
}

// QueryService interface for compatibility
type QueryService struct {
	lexicalClient  *query.RipgrepClient
	semanticClient *query.WeaviateClient
	merger         *query.ResultMerger
	cache          *cache.RedisCache
	metrics        *observability.Metrics
	tracer         *observability.Tracer
}

func NewQueryService(
	lexicalClient *query.RipgrepClient,
	semanticClient *query.WeaviateClient,
	merger *query.ResultMerger,
	cache *cache.RedisCache,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *QueryService {
	return &QueryService{
		lexicalClient:  lexicalClient,
		semanticClient: semanticClient,
		merger:         merger,
		cache:          cache,
		metrics:        metrics,
		tracer:         tracer,
	}
}

// Getter methods for QueryService clients
func (qs *QueryService) GetLexicalClient() *query.RipgrepClient {
	return qs.lexicalClient
}

func (qs *QueryService) GetSemanticClient() *query.WeaviateClient {
	return qs.semanticClient
}

// TODO: This function needs to be implemented properly with correct request type
// Mock implementation for GetContext - this would be replaced with actual streaming implementation
// func (q *QueryService) GetContext(ctx context.Context, req *SearchRequest) (Stream, error) {
//     // This is a simplified mock - in real implementation, this would be the actual GetContext service
//     // For now, return empty stream
//     return &MockStream{}, nil
// }

type Stream interface {
	Recv() (*repocontextv1.CodeChunk, error)
}

type MockStream struct {
	chunks []*repocontextv1.CodeChunk
	index  int
}

func (m *MockStream) Recv() (*repocontextv1.CodeChunk, error) {
	if m.index >= len(m.chunks) {
		return nil, fmt.Errorf("EOF")
	}
	chunk := m.chunks[m.index]
	m.index++
	return chunk, nil
}