package api

import (
	"context"

	"repo-context-service/internal/cache"
	"repo-context-service/internal/config"
	"repo-context-service/internal/ingest"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type RepositoryServer struct {
	repocontextv1.UnimplementedRepositoryServiceServer
	config         *config.Config
	cache          *cache.RedisCache
	ingestProvider ingest.Provider
	metrics        *observability.Metrics
	tracer         *observability.Tracer
}

func NewRepositoryServer(
	cfg *config.Config,
	cache *cache.RedisCache,
	ingestProvider ingest.Provider,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
) *RepositoryServer {
	return &RepositoryServer{
		config:         cfg,
		cache:          cache,
		ingestProvider: ingestProvider,
		metrics:        metrics,
		tracer:         tracer,
	}
}

func (s *RepositoryServer) ListRepositories(ctx context.Context, req *repocontextv1.ListRepositoriesRequest) (*repocontextv1.ListRepositoriesResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "ListRepositories")
	defer span.End()

	tenantID := req.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
	)

	// Get repositories from cache
	repositories, err := s.cache.ListRepositoryMetadata(ctx, tenantID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list repositories: %v", err)
	}

	// Simple pagination - in a real system, you'd want more sophisticated pagination
	pageSize := int(req.PageSize)
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20 // Default page size
	}

	startIdx := 0
	if req.PageToken != "" {
		// Parse page token (simplified - in real system use encoded tokens)
		// For now, we'll just skip this complex logic
	}

	endIdx := startIdx + pageSize
	if endIdx > len(repositories) {
		endIdx = len(repositories)
	}

	var pagedRepos []*repocontextv1.Repository
	if startIdx < len(repositories) {
		pagedRepos = repositories[startIdx:endIdx]
	}

	var nextPageToken string
	if endIdx < len(repositories) {
		nextPageToken = "next" // Simplified token
	}

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(pagedRepos)),
	)

	return &repocontextv1.ListRepositoriesResponse{
		Repositories:  pagedRepos,
		NextPageToken: nextPageToken,
	}, nil
}

func (s *RepositoryServer) GetRepository(ctx context.Context, req *repocontextv1.GetRepositoryRequest) (*repocontextv1.GetRepositoryResponse, error) {
	ctx, span := s.tracer.StartRPC(ctx, "GetRepository")
	defer span.End()

	tenantID := req.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
		observability.RepositoryAttr(req.RepositoryId),
	)

	// Get repository from cache
	repository, err := s.cache.GetRepositoryMetadata(ctx, tenantID, req.RepositoryId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get repository: %v", err)
	}

	if repository == nil {
		return nil, status.Errorf(codes.NotFound, "repository not found")
	}

	// Update ingestion status if needed
	if repository.IngestionStatus.State != repocontextv1.IngestionStatus_STATE_READY &&
		repository.IngestionStatus.State != repocontextv1.IngestionStatus_STATE_FAILED {
		// Check current status from ingestion provider
		currentStatus, err := s.ingestProvider.GetIndexStatus(ctx, req.RepositoryId)
		if err == nil && currentStatus != nil {
			repository.IngestionStatus = currentStatus
			// Update cache with new status
			s.cache.SetRepositoryMetadata(ctx, tenantID, repository)
		}
	}

	return &repocontextv1.GetRepositoryResponse{
		Repository: repository,
	}, nil
}

func (s *RepositoryServer) DeleteRepository(ctx context.Context, req *repocontextv1.DeleteRepositoryRequest) (*emptypb.Empty, error) {
	ctx, span := s.tracer.StartRPC(ctx, "DeleteRepository")
	defer span.End()

	tenantID := req.TenantId
	if tenantID == "" {
		tenantID = s.config.Security.DefaultTenant
	}

	observability.SetSpanAttributes(span,
		observability.TenantAttr(tenantID),
		observability.RepositoryAttr(req.RepositoryId),
	)

	// Get repository metadata first
	repository, err := s.cache.GetRepositoryMetadata(ctx, tenantID, req.RepositoryId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get repository: %v", err)
	}

	if repository == nil {
		return nil, status.Errorf(codes.NotFound, "repository not found")
	}

	// Delete from ingestion provider (vectors, etc.)
	if err := s.ingestProvider.DeleteIndex(ctx, req.RepositoryId); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete repository index: %v", err)
	}

	// Delete from cache
	if err := s.cache.DeleteRepositoryMetadata(ctx, tenantID, req.RepositoryId); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete repository metadata: %v", err)
	}

	// Delete repository routing if it exists
	if repository.Source != nil {
		repoKey := generateRepoKeyFromSource(repository.Source)
		s.cache.DeleteRepositoryIndex(ctx, tenantID, repoKey)
	}

	return &emptypb.Empty{}, nil
}

// Helper functions

func generateRepoKeyFromSource(source *repocontextv1.RepositorySource) string {
	switch src := source.Source.(type) {
	case *repocontextv1.RepositorySource_GitUrl:
		if source.CommitSha != "" {
			return src.GitUrl + "@" + source.CommitSha
		}
		return src.GitUrl + "@" + source.Ref
	case *repocontextv1.RepositorySource_UploadedFilename:
		return src.UploadedFilename
	default:
		return "unknown"
	}
}