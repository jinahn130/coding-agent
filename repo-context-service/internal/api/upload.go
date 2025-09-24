package api

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"repo-context-service/internal/cache"
	"repo-context-service/internal/config"
	"repo-context-service/internal/ingest"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type UploadServer struct {
	repocontextv1.UnimplementedUploadServiceServer
	config         *config.Config
	cache          *cache.RedisCache
	ingestProvider ingest.Provider
	metrics        *observability.Metrics
	tracer         *observability.Tracer
}

func NewUploadServer(
	cfg *config.Config,
	cache *cache.RedisCache,
	ingestProvider ingest.Provider,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *UploadServer {
	return &UploadServer{
		config:         cfg,
		cache:          cache,
		ingestProvider: ingestProvider,
		metrics:        metrics,
		tracer:         tracer,
	}
}

func (s *UploadServer) UploadRepository(stream repocontextv1.UploadService_UploadRepositoryServer) error {
	ctx := stream.Context()
	ctx, span := s.tracer.StartRPC(ctx, "UploadRepository")
	defer span.End()

	// Receive first message to get metadata
	firstReq, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive first request: %v", err)
	}

	// Extract tenant ID and validate
	tenantID := firstReq.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
	)

	// Generate repository ID
	repoID := generateRepositoryID()
	uploadID := firstReq.IdempotencyKey
	if uploadID == "" {
		uploadID = generateUploadID()
	}

	// Check idempotency
	if existing, err := s.cache.GetUploadStatus(ctx, tenantID, uploadID); err == nil && existing != nil {
		return stream.SendAndClose(&repocontextv1.UploadRepositoryResponse{
			UploadId:     existing.UploadID,
			RepositoryId: existing.RepositoryID,
			AcceptedAt:   timestamppb.New(existing.CreatedAt),
			Status:       existing.Status,
		})
	}

	timer := observability.StartTimer()
	defer func() {
		s.metrics.RecordIngestionDuration(timer.Duration())
	}()

	// Handle different source types
	var repositorySource *repocontextv1.RepositorySource

	switch source := firstReq.Source.(type) {
	case *repocontextv1.UploadRepositoryRequest_FileUpload:
		// Handle file upload
		filename, err := s.handleFileUpload(ctx, stream, firstReq, repoID)
		if err != nil {
			s.metrics.RecordUploadRequest("file", "error")
			return status.Errorf(codes.Internal, "file upload failed: %v", err)
		}

		repositorySource = &repocontextv1.RepositorySource{
			Source: &repocontextv1.RepositorySource_UploadedFilename{
				UploadedFilename: filename,
			},
			Ref: "main",
		}

		s.metrics.RecordUploadRequest("file", "success")

	case *repocontextv1.UploadRepositoryRequest_GitRepository:
		// Handle Git repository
		repositorySource = &repocontextv1.RepositorySource{
			Source: &repocontextv1.RepositorySource_GitUrl{
				GitUrl: source.GitRepository.Url,
			},
			Ref: source.GitRepository.Ref,
		}

		if repositorySource.Ref == "" {
			repositorySource.Ref = "main"
		}

		s.metrics.RecordUploadRequest("git", "success")

	default:
		return status.Errorf(codes.InvalidArgument, "unsupported source type")
	}

	// Create ingestion request
	ingestReq := &ingest.CreateIndexRequest{
		RepositoryID:   repoID,
		TenantID:       tenantID,
		Source:         repositorySource,
		Options:        firstReq.Options,
		IdempotencyKey: uploadID,
		ProgressCallback: func(progress *repocontextv1.IngestionProgress) {
			// Progress callback - could be used for real-time updates
			// For now, we'll store it in cache
		},
	}

	// Start ingestion
	ingestResp, err := s.ingestProvider.CreateRepositoryIndex(ctx, ingestReq)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to start ingestion: %v", err)
	}

	// Send response
	response := &repocontextv1.UploadRepositoryResponse{
		UploadId:     uploadID,
		RepositoryId: repoID,
		AcceptedAt:   timestamppb.New(ingestResp.AcceptedAt),
		Status:       ingestResp.Status,
	}

	observability.SetSpanAttributes(span,
		observability.RepositoryAttr(repoID),
	)

	return stream.SendAndClose(response)
}

func (s *UploadServer) handleFileUpload(
	ctx context.Context,
	stream repocontextv1.UploadService_UploadRepositoryServer,
	firstReq *repocontextv1.UploadRepositoryRequest,
	repoID string,
) (string, error) {
	fileUpload := firstReq.GetFileUpload()
	if fileUpload == nil {
		return "", fmt.Errorf("no file upload data in first request")
	}

	// Create temp file
	tempDir := s.config.Upload.TempDir
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	filename := fileUpload.Filename
	if filename == "" {
		filename = fmt.Sprintf("upload-%s", repoID)
	}

	tempFile := filepath.Join(tempDir, filename)
	file, err := os.Create(tempFile)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer file.Close()

	totalSize := int64(0)
	chunkCount := 0

	// Write first chunk
	if len(fileUpload.Chunk) > 0 {
		n, err := file.Write(fileUpload.Chunk)
		if err != nil {
			return "", fmt.Errorf("failed to write first chunk: %w", err)
		}
		totalSize += int64(n)
		chunkCount++
	}

	// If not final, continue receiving chunks
	if !fileUpload.IsFinal {
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("failed to receive chunk: %w", err)
			}

			fileUpload := req.GetFileUpload()
			if fileUpload == nil {
				continue
			}

			// Write chunk
			if len(fileUpload.Chunk) > 0 {
				n, err := file.Write(fileUpload.Chunk)
				if err != nil {
					return "", fmt.Errorf("failed to write chunk %d: %w", chunkCount, err)
				}
				totalSize += int64(n)
			}
			chunkCount++

			// Check size limits
			if totalSize > s.config.Upload.MaxFileSize {
				return "", fmt.Errorf("file too large: %d bytes exceeds limit of %d bytes", totalSize, s.config.Upload.MaxFileSize)
			}

			if fileUpload.IsFinal {
				break
			}
		}
	}

	// Record upload size
	s.metrics.RecordUploadSize(totalSize)

	return filename, nil
}

func (s *UploadServer) GetUploadStatus(ctx context.Context, req *repocontextv1.GetUploadStatusRequest) (*repocontextv1.GetUploadStatusResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "GetUploadStatus")
	defer span.End()

	tenantID := req.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
	)

	// Get status from cache
	uploadStatus, err := s.cache.GetUploadStatus(ctx, tenantID, req.UploadId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get upload status: %v", err)
	}

	if uploadStatus == nil {
		return nil, status.Errorf(codes.NotFound, "upload not found")
	}

	response := &repocontextv1.GetUploadStatusResponse{
		UploadId:     uploadStatus.UploadID,
		RepositoryId: uploadStatus.RepositoryID,
		Status:       uploadStatus.Status,
		Progress:     uploadStatus.Progress,
		ErrorMessage: uploadStatus.ErrorMessage,
	}

	return response, nil
}

func (s *UploadServer) UploadGitRepository(ctx context.Context, req *repocontextv1.UploadGitRepositoryRequest) (*repocontextv1.UploadRepositoryResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "UploadGitRepository")
	defer span.End()

	// Extract tenant ID and validate
	tenantID := req.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
	)

	// Generate repository ID
	repoID := generateRepositoryID()
	uploadID := req.IdempotencyKey
	if uploadID == "" {
		uploadID = generateUploadID()
	}

	// TEMPORARILY DISABLED: Check idempotency (disabled to force processing)
	// This was preventing actual repository processing by returning cached empty results
	/*
	if existing, err := s.cache.GetUploadStatus(ctx, tenantID, uploadID); err == nil && existing != nil {
		return &repocontextv1.UploadRepositoryResponse{
			UploadId:     existing.UploadID,
			RepositoryId: existing.RepositoryID,
			AcceptedAt:   timestamppb.New(existing.CreatedAt),
			Status:       existing.Status,
		}, nil
	}
	*/

	timer := observability.StartTimer()
	defer func() {
		s.metrics.RecordIngestionDuration(timer.Duration())
	}()

	// Validate Git repository
	gitRepo := req.GitRepository
	if gitRepo == nil {
		return nil, status.Errorf(codes.InvalidArgument, "git_repository is required")
	}
	if gitRepo.Url == "" {
		return nil, status.Errorf(codes.InvalidArgument, "git_repository.url is required")
	}

	// Create repository source
	repositorySource := &repocontextv1.RepositorySource{
		Source: &repocontextv1.RepositorySource_GitUrl{
			GitUrl: gitRepo.Url,
		},
		Ref: gitRepo.Ref,
	}

	if repositorySource.Ref == "" {
		repositorySource.Ref = "main"
	}

	// Create ingestion request
	ingestReq := &ingest.CreateIndexRequest{
		RepositoryID:   repoID,
		TenantID:       tenantID,
		Source:         repositorySource,
		Options:        req.Options,
		IdempotencyKey: uploadID,
		ProgressCallback: func(progress *repocontextv1.IngestionProgress) {
			// Progress callback - could be used for real-time updates
		},
	}

	// Start ingestion
	ingestResp, err := s.ingestProvider.CreateRepositoryIndex(ctx, ingestReq)
	if err != nil {
		s.metrics.RecordUploadRequest("git", "error")
		return nil, status.Errorf(codes.Internal, "failed to start ingestion: %v", err)
	}

	// Create repository metadata for listing
	repository := &repocontextv1.Repository{
		RepositoryId: repoID,
		Name:         extractRepositoryName(gitRepo.Url),
		Description:  fmt.Sprintf("Repository cloned from %s", gitRepo.Url),
		Source:       repositorySource,
		IngestionStatus: ingestResp.Status,
		Stats: &repocontextv1.RepositoryStats{
			TotalFiles:  0,
			TotalLines:  0,
			TotalChunks: 0,
			SizeBytes:   0,
		},
		CreatedAt: timestamppb.New(ingestResp.AcceptedAt),
		UpdatedAt: timestamppb.New(ingestResp.AcceptedAt),
	}

	// Store repository metadata in cache
	if err := s.cache.SetRepositoryMetadata(ctx, tenantID, repository); err != nil {
		// Log error but don't fail the upload
		fmt.Printf("Warning: failed to store repository metadata: %v\n", err)
	}

	s.metrics.RecordUploadRequest("git", "success")

	// Send response
	response := &repocontextv1.UploadRepositoryResponse{
		UploadId:     uploadID,
		RepositoryId: repoID,
		AcceptedAt:   timestamppb.New(ingestResp.AcceptedAt),
		Status:       ingestResp.Status,
	}

	observability.SetSpanAttributes(span,
		observability.RepositoryAttr(repoID),
	)

	return response, nil
}

// Helper functions

func generateRepositoryID() string {
	return fmt.Sprintf("repo-%d", time.Now().UnixNano())
}

func extractRepositoryName(gitUrl string) string {
	// Extract repository name from Git URL
	// e.g., "https://github.com/user/repo.git" -> "repo"
	//       "https://github.com/user/repo" -> "repo"

	// Remove .git suffix if present
	url := gitUrl
	if len(url) > 4 && url[len(url)-4:] == ".git" {
		url = url[:len(url)-4]
	}

	// Find last slash and extract name
	lastSlash := len(url) - 1
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			lastSlash = i
			break
		}
	}

	if lastSlash < len(url)-1 {
		return url[lastSlash+1:]
	}

	return "repository"
}

func generateUploadID() string {
	return fmt.Sprintf("upload-%d", time.Now().UnixNano())
}