package ingest

import (
	"context"
	"time"

	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
)

type Provider interface {
	CreateRepositoryIndex(ctx context.Context, req *CreateIndexRequest) (*CreateIndexResponse, error)
	GetIndexStatus(ctx context.Context, repoID string) (*repocontextv1.IngestionStatus, error)
	DeleteIndex(ctx context.Context, repoID string) error
}

type CreateIndexRequest struct {
	RepositoryID    string
	TenantID        string
	Source          *repocontextv1.RepositorySource
	Options         *repocontextv1.UploadOptions
	IdempotencyKey  string
	ProgressCallback func(*repocontextv1.IngestionProgress)
}

type CreateIndexResponse struct {
	RepositoryID string
	IndexID      string
	Status       *repocontextv1.IngestionStatus
	AcceptedAt   time.Time
}

type RepositoryProcessor interface {
	ExtractRepository(ctx context.Context, source *repocontextv1.RepositorySource, targetDir string) (*ExtractResult, error)
	ChunkFiles(ctx context.Context, extractResult *ExtractResult, options *ChunkOptions) ([]*FileChunk, error)
	GenerateEmbeddings(ctx context.Context, chunks []*FileChunk) ([]*EmbeddedChunk, error)
	IndexEmbeddings(ctx context.Context, repoID string, chunks []*EmbeddedChunk) error
}

type ExtractResult struct {
	RepositoryPath string
	CommitSHA      string
	Files          []*FileInfo
	Stats          *repocontextv1.RepositoryStats
}

type FileInfo struct {
	Path         string
	Size         int64
	Language     string
	IsText       bool
	IsBinary     bool
	LineCount    int
	LastModified time.Time
}

type ChunkOptions struct {
	ChunkSize    int
	ChunkOverlap int
	ExcludePatterns []string
	IncludePatterns []string
	MaxFileSize  int64
}

type FileChunk struct {
	ID           string
	RepositoryID string
	FilePath     string
	StartLine    int
	EndLine      int
	Content      string
	Language     string
	Size         int
	Hash         string
}

type EmbeddedChunk struct {
	*FileChunk
	Embedding []float32
	Model     string
	CreatedAt time.Time
}

type IngestionJob struct {
	ID           string
	RepositoryID string
	TenantID     string
	Status       *repocontextv1.IngestionStatus
	Progress     *repocontextv1.IngestionProgress
	Request      *CreateIndexRequest
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ErrorMessage string
}

type JobManager interface {
	SubmitJob(ctx context.Context, job *IngestionJob) error
	GetJob(ctx context.Context, jobID string) (*IngestionJob, error)
	UpdateJobStatus(ctx context.Context, jobID string, status *repocontextv1.IngestionStatus) error
	UpdateJobProgress(ctx context.Context, jobID string, progress *repocontextv1.IngestionProgress) error
	SetJobError(ctx context.Context, jobID string, err error) error
	ListJobs(ctx context.Context, tenantID string, limit int, offset int) ([]*IngestionJob, error)
}

type ProgressTracker struct {
	Total     int32
	Processed int32
	callback  func(*repocontextv1.IngestionProgress)
}

func NewProgressTracker(total int32, callback func(*repocontextv1.IngestionProgress)) *ProgressTracker {
	return &ProgressTracker{
		Total:    total,
		callback: callback,
	}
}

func (pt *ProgressTracker) Update(processed int32) {
	pt.Processed = processed
	if pt.callback != nil {
		progress := &repocontextv1.IngestionProgress{
			TotalFiles:      pt.Total,
			ProcessedFiles:  pt.Processed,
			ProgressPercent: float32(pt.Processed) / float32(pt.Total) * 100,
		}
		pt.callback(progress)
	}
}

func (pt *ProgressTracker) Increment() {
	pt.Update(pt.Processed + 1)
}

func (pt *ProgressTracker) SetCounts(totalFiles, processedFiles, totalChunks, embeddedChunks, indexedChunks int32) {
	if pt.callback != nil {
		progress := &repocontextv1.IngestionProgress{
			TotalFiles:      totalFiles,
			ProcessedFiles:  processedFiles,
			TotalChunks:     totalChunks,
			EmbeddedChunks:  embeddedChunks,
			IndexedChunks:   indexedChunks,
			ProgressPercent: calculateProgress(totalFiles, processedFiles, totalChunks, embeddedChunks, indexedChunks),
		}
		pt.callback(progress)
	}
}

func calculateProgress(totalFiles, processedFiles, totalChunks, embeddedChunks, indexedChunks int32) float32 {
	if totalFiles == 0 {
		return 0
	}

	// Weight the different phases
	fileProcessingWeight := 0.3
	embeddingWeight := 0.5
	indexingWeight := 0.2

	fileProgress := float32(processedFiles) / float32(totalFiles)

	var embeddingProgress float32
	if totalChunks > 0 {
		embeddingProgress = float32(embeddedChunks) / float32(totalChunks)
	}

	var indexingProgress float32
	if totalChunks > 0 {
		indexingProgress = float32(indexedChunks) / float32(totalChunks)
	}

	totalProgress := (fileProgress * float32(fileProcessingWeight)) +
		(embeddingProgress * float32(embeddingWeight)) +
		(indexingProgress * float32(indexingWeight))

	return totalProgress * 100
}