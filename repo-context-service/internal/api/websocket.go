package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc"

	"repo-context-service/internal/config"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
)

// WebSocket message types that match the JavaScript client expectations
type WSMessage struct {
	Start       *WSChatStart   `json:"start,omitempty"`
	ChatMessage *WSChatMessage `json:"chat_message,omitempty"`
	Cancel      *WSChatCancel  `json:"cancel,omitempty"`
}

type WSChatStart struct {
	RepositoryID string            `json:"repository_id"`
	TenantID     string            `json:"tenant_id"`
	Options      *WSChatOptions    `json:"options,omitempty"`
}

type WSChatMessage struct {
	Query     string `json:"query"`
	SessionID string `json:"session_id"`
}

type WSChatCancel struct {
	SessionID string `json:"session_id"`
}

type WSChatOptions struct {
	MaxResults   int32  `json:"max_results"`
	StreamTokens bool   `json:"stream_tokens"`
	Model        string `json:"model"`
}

// WebSocket response types that match JavaScript client expectations
type WSResponse struct {
	SearchStarted       *WSSearchStarted       `json:"search_started,omitempty"`
	SearchHit           *WSSearchHit           `json:"search_hit,omitempty"`
	CompositionStarted  *WSCompositionStarted  `json:"composition_started,omitempty"`
	CompositionToken    *WSCompositionToken    `json:"composition_token,omitempty"`
	CompositionComplete *WSCompositionComplete `json:"composition_complete,omitempty"`
	Error               *WSError               `json:"error,omitempty"`
	Complete            *WSComplete            `json:"complete,omitempty"`
}

type WSSearchStarted struct {
	SessionID string `json:"session_id"`
	QueryID   string `json:"query_id"`
}

type WSSearchHit struct {
	SessionID string `json:"session_id"`
	QueryID   string `json:"query_id"`
	Phase     string `json:"phase"`
	Rank      int32  `json:"rank"`
	Chunk     *WSCodeChunk `json:"chunk"`
}

type WSCompositionStarted struct {
	SessionID     string `json:"session_id"`
	QueryID       string `json:"query_id"`
	ContextChunks int32  `json:"context_chunks"`
}

type WSCompositionToken struct {
	SessionID string `json:"session_id"`
	QueryID   string `json:"query_id"`
	Text      string `json:"text"`
}

type WSCompositionComplete struct {
	SessionID    string `json:"session_id"`
	QueryID      string `json:"query_id"`
	FullResponse string `json:"full_response"`
}

type WSError struct {
	SessionID    string `json:"session_id"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

type WSComplete struct {
	SessionID string `json:"session_id"`
	QueryID   string `json:"query_id"`
}

type WSCodeChunk struct {
	RepositoryID string  `json:"repository_id"`
	FilePath     string  `json:"file_path"`
	Content      string  `json:"content"`
	StartLine    int32   `json:"start_line"`
	EndLine      int32   `json:"end_line"`
	Language     string  `json:"language"`
	Score        float32 `json:"score"`
}

type ChatWebSocketHandler struct {
	upgrader   websocket.Upgrader
	chatServer *ChatServer
	config     *config.Config
	metrics    *observability.Metrics
	tracer     *observability.Tracer

	// Connection management
	connections map[string]*websocket.Conn
	connMutex   sync.RWMutex
}

func NewChatWebSocketHandler(
	chatServer *ChatServer,
	cfg *config.Config,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *ChatWebSocketHandler {
	return &ChatWebSocketHandler{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow connections from any origin for development
				// In production, this should be more restrictive
				return true
			},
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
		chatServer:  chatServer,
		config:      cfg,
		metrics:     metrics,
		tracer:      tracer,
		connections: make(map[string]*websocket.Conn),
	}
}

func (h *ChatWebSocketHandler) RegisterRoutes(router *mux.Router) {
	router.HandleFunc("/v1/chat/{repository_id}/stream", h.HandleWebSocket).Methods("GET")
}

func (h *ChatWebSocketHandler) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	repositoryID := vars["repository_id"]

	if repositoryID == "" {
		http.Error(w, "repository_id is required", http.StatusBadRequest)
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	connID := fmt.Sprintf("%s_%d", repositoryID, time.Now().UnixNano())

	// Register connection
	h.connMutex.Lock()
	h.connections[connID] = conn
	h.connMutex.Unlock()

	// Clean up on exit
	defer func() {
		h.connMutex.Lock()
		delete(h.connections, connID)
		h.connMutex.Unlock()
		conn.Close()
	}()

	log.Printf("WebSocket connection established for repository: %s", repositoryID)

	// Handle the WebSocket connection
	h.handleConnection(conn, repositoryID)
}

func (h *ChatWebSocketHandler) handleConnection(conn *websocket.Conn, repositoryID string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create gRPC client stream
	grpcConn, err := grpc.DialContext(ctx, "localhost:9090", grpc.WithInsecure())
	if err != nil {
		log.Printf("Failed to connect to gRPC server: %v", err)
		h.sendError(conn, "", "connection_failed", "Failed to connect to chat service")
		return
	}
	defer grpcConn.Close()

	client := repocontextv1.NewChatServiceClient(grpcConn)
	stream, err := client.ChatWithRepository(ctx)
	if err != nil {
		log.Printf("Failed to create gRPC stream: %v", err)
		h.sendError(conn, "", "stream_failed", "Failed to create chat stream")
		return
	}

	// Channel for coordinating goroutines
	done := make(chan bool)

	// Goroutine to read gRPC responses and send to WebSocket
	go h.grpcToWebSocket(stream, conn, done)

	// Main goroutine reads WebSocket messages and sends to gRPC
	h.webSocketToGRPC(conn, stream, repositoryID, done)
}

func (h *ChatWebSocketHandler) webSocketToGRPC(
	wsConn *websocket.Conn,
	grpcStream repocontextv1.ChatService_ChatWithRepositoryClient,
	_ string, // repositoryID unused but kept for interface compatibility
	done chan bool,
) {
	defer func() {
		done <- true
	}()

	for {
		// Read message from WebSocket
		var wsMsg WSMessage
		err := wsConn.ReadJSON(&wsMsg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			return
		}

		// Convert WebSocket message to gRPC message
		var grpcReq *repocontextv1.ChatRequest

		if wsMsg.Start != nil {
			grpcReq = &repocontextv1.ChatRequest{
				Message: &repocontextv1.ChatRequest_Start{
					Start: &repocontextv1.ChatStart{
						RepositoryId: wsMsg.Start.RepositoryID,
						TenantId:     wsMsg.Start.TenantID,
						Options: &repocontextv1.ChatOptions{
							MaxResults:   wsMsg.Start.Options.MaxResults,
							StreamTokens: wsMsg.Start.Options.StreamTokens,
							Model:        wsMsg.Start.Options.Model,
						},
					},
				},
			}
		} else if wsMsg.ChatMessage != nil {
			grpcReq = &repocontextv1.ChatRequest{
				Message: &repocontextv1.ChatRequest_ChatMessage{
					ChatMessage: &repocontextv1.ChatMessage{
						Query:     wsMsg.ChatMessage.Query,
						SessionId: wsMsg.ChatMessage.SessionID,
					},
				},
			}
		} else if wsMsg.Cancel != nil {
			grpcReq = &repocontextv1.ChatRequest{
				Message: &repocontextv1.ChatRequest_Cancel{
					Cancel: &repocontextv1.ChatCancel{
						SessionId: wsMsg.Cancel.SessionID,
					},
				},
			}
		} else {
			log.Printf("Unknown WebSocket message type")
			continue
		}

		// Send to gRPC stream
		err = grpcStream.Send(grpcReq)
		if err != nil {
			log.Printf("gRPC send error: %v", err)
			return
		}
	}
}

func (h *ChatWebSocketHandler) grpcToWebSocket(
	grpcStream repocontextv1.ChatService_ChatWithRepositoryClient,
	wsConn *websocket.Conn,
	done chan bool,
) {
	defer func() {
		done <- true
	}()

	for {
		select {
		case <-done:
			return
		default:
			// Read from gRPC stream
			grpcResp, err := grpcStream.Recv()
			if err != nil {
				log.Printf("gRPC receive error: %v", err)
				return
			}

			// Convert gRPC response to WebSocket response
			wsResp := h.convertGRPCToWebSocket(grpcResp)

			// Send to WebSocket
			err = wsConn.WriteJSON(wsResp)
			if err != nil {
				log.Printf("WebSocket write error: %v", err)
				return
			}
		}
	}
}

func (h *ChatWebSocketHandler) convertGRPCToWebSocket(grpcResp *repocontextv1.ChatResponse) *WSResponse {
	wsResp := &WSResponse{}

	switch msg := grpcResp.Message.(type) {
	case *repocontextv1.ChatResponse_SearchStarted:
		wsResp.SearchStarted = &WSSearchStarted{
			SessionID: msg.SearchStarted.SessionId,
			QueryID:   msg.SearchStarted.QueryId,
		}

	case *repocontextv1.ChatResponse_SearchHit:
		phase := "EARLY"
		if msg.SearchHit.Phase == repocontextv1.HitPhase_HIT_PHASE_FINAL {
			phase = "FINAL"
		}

		wsResp.SearchHit = &WSSearchHit{
			SessionID: msg.SearchHit.SessionId,
			QueryID:   msg.SearchHit.QueryId,
			Phase:     phase,
			Rank:      msg.SearchHit.Rank,
			Chunk:     h.convertCodeChunk(msg.SearchHit.Chunk),
		}

	case *repocontextv1.ChatResponse_CompositionStarted:
		wsResp.CompositionStarted = &WSCompositionStarted{
			SessionID:     msg.CompositionStarted.SessionId,
			QueryID:       msg.CompositionStarted.QueryId,
			ContextChunks: msg.CompositionStarted.ContextChunks,
		}

	case *repocontextv1.ChatResponse_CompositionToken:
		wsResp.CompositionToken = &WSCompositionToken{
			SessionID: msg.CompositionToken.SessionId,
			QueryID:   msg.CompositionToken.QueryId,
			Text:      msg.CompositionToken.Text,
		}

	case *repocontextv1.ChatResponse_CompositionComplete:
		wsResp.CompositionComplete = &WSCompositionComplete{
			SessionID:    msg.CompositionComplete.SessionId,
			QueryID:      msg.CompositionComplete.QueryId,
			FullResponse: msg.CompositionComplete.FullResponse,
		}

	case *repocontextv1.ChatResponse_Error:
		wsResp.Error = &WSError{
			SessionID:    msg.Error.SessionId,
			ErrorCode:    msg.Error.ErrorCode,
			ErrorMessage: msg.Error.ErrorMessage,
		}

	case *repocontextv1.ChatResponse_Complete:
		wsResp.Complete = &WSComplete{
			SessionID: msg.Complete.SessionId,
			QueryID:   msg.Complete.QueryId,
		}
	}

	return wsResp
}

func (h *ChatWebSocketHandler) convertCodeChunk(chunk *repocontextv1.CodeChunk) *WSCodeChunk {
	if chunk == nil {
		return nil
	}

	return &WSCodeChunk{
		RepositoryID: chunk.RepositoryId,
		FilePath:     chunk.FilePath,
		Content:      chunk.Content,
		StartLine:    chunk.StartLine,
		EndLine:      chunk.EndLine,
		Language:     chunk.Language,
		Score:        chunk.Score,
	}
}

func (h *ChatWebSocketHandler) sendError(conn *websocket.Conn, sessionID, errorCode, errorMessage string) {
	response := &WSResponse{
		Error: &WSError{
			SessionID:    sessionID,
			ErrorCode:    errorCode,
			ErrorMessage: errorMessage,
		},
	}

	conn.WriteJSON(response)
}