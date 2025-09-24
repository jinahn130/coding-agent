package ingest

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"repo-context-service/internal/cache"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type InlineProcessor struct {
	cache         *cache.RedisCache
	metrics       *observability.Metrics
	tracer        *observability.Tracer
	embeddingClient EmbeddingClient
	vectorClient    VectorClient
	workDir       string
	tempDir       string
}

type EmbeddingClient interface {
	GenerateEmbeddings(ctx context.Context, texts []string, model string) ([][]float32, error)
	GetDefaultModel() string
}

type VectorClient interface {
	CreateCollection(ctx context.Context, name string, dimensions int) error
	UpsertVectors(ctx context.Context, collectionName string, vectors []*Vector) error
	DeleteCollection(ctx context.Context, name string) error
}

type Vector struct {
	ID       string
	Vector   []float32
	Metadata map[string]interface{}
}

func NewInlineProcessor(
	cache *cache.RedisCache,
	metrics *observability.Metrics,
	tracer *observability.Tracer,
	embeddingClient EmbeddingClient,
	vectorClient VectorClient,
	workDir, tempDir string,
) *InlineProcessor {
	return &InlineProcessor{
		cache:           cache,
		metrics:         metrics,
		tracer:          tracer,
		embeddingClient: embeddingClient,
		vectorClient:    vectorClient,
		workDir:         workDir,
		tempDir:         tempDir,
	}
}

func (ip *InlineProcessor) CreateRepositoryIndex(ctx context.Context, req *CreateIndexRequest) (*CreateIndexResponse, error) {
	ctx, span := ip.tracer.StartIngestion(ctx, req.RepositoryID, "create_index")
	defer span.End()

	// TEMPORARILY DISABLED: Check idempotency (disabled to force processing)
	// This was preventing actual repository processing by returning cached empty results
	/*
	if existing, err := ip.cache.GetUploadStatus(ctx, req.TenantID, req.IdempotencyKey); err == nil && existing != nil {
		return &CreateIndexResponse{
			RepositoryID: existing.RepositoryID,
			IndexID:      existing.RepositoryID, // Use repo ID as index ID for simplicity
			Status:       existing.Status,
			AcceptedAt:   existing.CreatedAt,
		}, nil
	}
	*/

	// Create job
	job := &IngestionJob{
		ID:           req.IdempotencyKey,
		RepositoryID: req.RepositoryID,
		TenantID:     req.TenantID,
		Status: &repocontextv1.IngestionStatus{
			State:     repocontextv1.IngestionStatus_STATE_PENDING,
			UpdatedAt: timestamppb.Now(),
		},
		Progress: &repocontextv1.IngestionProgress{
			ProgressPercent: 0,
		},
		Request:   req,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	// Cache initial status
	cachedStatus := &cache.CachedUploadStatus{
		UploadID:     req.IdempotencyKey,
		RepositoryID: req.RepositoryID,
		Status:       job.Status,
		Progress:     job.Progress,
		CreatedAt:    job.CreatedAt,
	}

	if err := ip.cache.SetUploadStatus(ctx, req.TenantID, cachedStatus); err != nil {
		return nil, fmt.Errorf("failed to cache upload status: %w", err)
	}

	// Start ingestion in background
	go ip.processRepositoryAsync(context.Background(), job)

	return &CreateIndexResponse{
		RepositoryID: req.RepositoryID,
		IndexID:      req.RepositoryID,
		Status:       job.Status,
		AcceptedAt:   job.CreatedAt,
	}, nil
}

func (ip *InlineProcessor) processRepositoryAsync(ctx context.Context, job *IngestionJob) {
	timer := observability.StartTimer()
	defer func() {
		ip.metrics.RecordIngestionDuration(timer.Duration())
	}()

	if err := ip.processRepository(ctx, job); err != nil {
		job.Status.State = repocontextv1.IngestionStatus_STATE_FAILED
		job.ErrorMessage = err.Error()
		job.UpdatedAt = time.Now()

		// Update cache with error
		cachedStatus := &cache.CachedUploadStatus{
			UploadID:     job.ID,
			RepositoryID: job.RepositoryID,
			Status:       job.Status,
			Progress:     job.Progress,
			ErrorMessage: job.ErrorMessage,
			CreatedAt:    job.CreatedAt,
		}
		ip.cache.SetUploadStatus(ctx, job.TenantID, cachedStatus)
	}
}

func (ip *InlineProcessor) processRepository(ctx context.Context, job *IngestionJob) error {
	req := job.Request

	log.Printf("processRepository: Starting processing for repository %s", req.RepositoryID)

	// Update status to extracting
	job.Status.State = repocontextv1.IngestionStatus_STATE_EXTRACTING
	ip.updateJobStatus(ctx, job)

	// Extract repository
	extractResult, err := ip.ExtractRepository(ctx, req.Source, filepath.Join(ip.workDir, req.RepositoryID))
	if err != nil {
		return fmt.Errorf("failed to extract repository: %w", err)
	}

	// Update status to chunking
	job.Status.State = repocontextv1.IngestionStatus_STATE_CHUNKING
	ip.updateJobStatus(ctx, job)

	// Create progress tracker
	progressTracker := NewProgressTracker(int32(len(extractResult.Files)), req.ProgressCallback)

	// Chunk files
	var excludePatterns []string
	var includePatterns []string
	var maxFileSizeMb int32 = 10 // Default 10MB

	if req.Options != nil {
		excludePatterns = req.Options.ExcludePatterns
		includePatterns = req.Options.IncludePatterns
		if req.Options.MaxFileSizeMb > 0 {
			maxFileSizeMb = req.Options.MaxFileSizeMb
		}
	}

	chunkOptions := &ChunkOptions{
		ChunkSize:       100,
		ChunkOverlap:    10,
		ExcludePatterns: excludePatterns,
		IncludePatterns: includePatterns,
		MaxFileSize:     int64(maxFileSizeMb) * 1024 * 1024,
	}

	log.Printf("processRepository: About to start chunking %d files", len(extractResult.Files))
	chunks, err := ip.ChunkFiles(ctx, extractResult, chunkOptions)
	if err != nil {
		log.Printf("processRepository: ChunkFiles failed: %v", err)
		return fmt.Errorf("failed to chunk files: %w", err)
	}

	log.Printf("processRepository: ChunkFiles completed successfully. Created %d chunks from %d files",
		len(chunks), len(extractResult.Files))

	progressTracker.SetCounts(int32(len(extractResult.Files)), int32(len(extractResult.Files)), int32(len(chunks)), 0, 0)

	// Update status to embedding
	job.Status.State = repocontextv1.IngestionStatus_STATE_EMBEDDING
	ip.updateJobStatus(ctx, job)

	log.Printf("processRepository: About to generate embeddings for %d chunks", len(chunks))
	// Generate embeddings
	embeddedChunks, err := ip.GenerateEmbeddings(ctx, chunks)
	if err != nil {
		log.Printf("processRepository: GenerateEmbeddings failed: %v", err)
		return fmt.Errorf("failed to generate embeddings: %w", err)
	}

	log.Printf("processRepository: GenerateEmbeddings completed. Generated embeddings for %d chunks", len(embeddedChunks))

	progressTracker.SetCounts(int32(len(extractResult.Files)), int32(len(extractResult.Files)), int32(len(chunks)), int32(len(embeddedChunks)), 0)

	// Update status to indexing
	job.Status.State = repocontextv1.IngestionStatus_STATE_INDEXING
	ip.updateJobStatus(ctx, job)

	log.Printf("processRepository: About to index %d embedded chunks to Weaviate", len(embeddedChunks))
	// Index embeddings
	if err := ip.IndexEmbeddings(ctx, req.RepositoryID, embeddedChunks); err != nil {
		log.Printf("processRepository: IndexEmbeddings failed: %v", err)
		return fmt.Errorf("failed to index embeddings: %w", err)
	}

	log.Printf("processRepository: IndexEmbeddings completed successfully")

	progressTracker.SetCounts(int32(len(extractResult.Files)), int32(len(extractResult.Files)), int32(len(chunks)), int32(len(embeddedChunks)), int32(len(embeddedChunks)))

	// Update status to ready
	job.Status.State = repocontextv1.IngestionStatus_STATE_READY
	job.Progress.ProgressPercent = 100
	ip.updateJobStatus(ctx, job)

	// Store repository metadata
	repository := &repocontextv1.Repository{
		RepositoryId:    req.RepositoryID,
		Name:            extractRepositoryName(req.Source),
		Source:          req.Source,
		IngestionStatus: job.Status,
		Stats:           extractResult.Stats,
		CreatedAt:       timestamppb.New(job.CreatedAt),
		UpdatedAt:       timestamppb.Now(),
	}

	if err := ip.cache.SetRepositoryMetadata(ctx, req.TenantID, repository); err != nil {
		return fmt.Errorf("failed to store repository metadata: %w", err)
	}

	// Set repository routing
	repoKey := generateRepoKey(req.Source)
	if err := ip.cache.SetRepositoryIndex(ctx, req.TenantID, repoKey, req.RepositoryID); err != nil {
		return fmt.Errorf("failed to set repository routing: %w", err)
	}

	return nil
}

func (ip *InlineProcessor) ExtractRepository(ctx context.Context, source *repocontextv1.RepositorySource, targetDir string) (*ExtractResult, error) {
	ctx, span := ip.tracer.StartIngestion(ctx, "", "extract")
	defer span.End()

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create target directory: %w", err)
	}

	var commitSHA string
	var err error

	switch src := source.Source.(type) {
	case *repocontextv1.RepositorySource_GitUrl:
		commitSHA, err = ip.cloneGitRepository(ctx, src.GitUrl, source.Ref, targetDir)
	case *repocontextv1.RepositorySource_UploadedFilename:
		commitSHA, err = ip.extractUploadedFile(ctx, src.UploadedFilename, targetDir)
	default:
		return nil, fmt.Errorf("unsupported repository source type")
	}

	if err != nil {
		return nil, err
	}

	// Scan files
	files, stats, err := ip.scanDirectory(ctx, targetDir)
	if err != nil {
		return nil, fmt.Errorf("failed to scan directory: %w", err)
	}

	return &ExtractResult{
		RepositoryPath: targetDir,
		CommitSHA:      commitSHA,
		Files:          files,
		Stats:          stats,
	}, nil
}

func (ip *InlineProcessor) cloneGitRepository(ctx context.Context, gitURL, ref, targetDir string) (string, error) {
	if ref == "" {
		ref = "main"
	}

	// Shallow clone
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--branch", ref, gitURL, targetDir)
	if err := cmd.Run(); err != nil {
		// Try master if main fails
		if ref == "main" {
			cmd = exec.CommandContext(ctx, "git", "clone", "--depth=1", "--branch", "master", gitURL, targetDir)
			if err := cmd.Run(); err != nil {
				return "", fmt.Errorf("failed to clone repository: %w", err)
			}
		} else {
			return "", fmt.Errorf("failed to clone repository: %w", err)
		}
	}

	// Get commit SHA
	cmd = exec.CommandContext(ctx, "git", "-C", targetDir, "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get commit SHA: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func (ip *InlineProcessor) extractUploadedFile(ctx context.Context, filename, targetDir string) (string, error) {
	filePath := filepath.Join(ip.tempDir, filename)

	switch {
	case strings.HasSuffix(filename, ".zip"):
		return ip.extractZip(filePath, targetDir)
	case strings.HasSuffix(filename, ".tar.gz") || strings.HasSuffix(filename, ".tgz"):
		return ip.extractTarGz(filePath, targetDir)
	case strings.HasSuffix(filename, ".tar"):
		return ip.extractTar(filePath, targetDir)
	default:
		return "", fmt.Errorf("unsupported file format: %s", filename)
	}
}

func (ip *InlineProcessor) extractZip(filePath, targetDir string) (string, error) {
	reader, err := zip.OpenReader(filePath)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	for _, file := range reader.File {
		path := filepath.Join(targetDir, file.Name)

		if file.FileInfo().IsDir() {
			os.MkdirAll(path, file.FileInfo().Mode())
			continue
		}

		fileReader, err := file.Open()
		if err != nil {
			return "", err
		}

		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			fileReader.Close()
			return "", err
		}

		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.FileInfo().Mode())
		if err != nil {
			fileReader.Close()
			return "", err
		}

		_, err = io.Copy(targetFile, fileReader)
		fileReader.Close()
		targetFile.Close()

		if err != nil {
			return "", err
		}
	}

	return ip.calculateDirectoryHash(targetDir)
}

func (ip *InlineProcessor) extractTarGz(filePath, targetDir string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return "", err
	}
	defer gzReader.Close()

	return ip.extractTarReader(tar.NewReader(gzReader), targetDir)
}

func (ip *InlineProcessor) extractTar(filePath, targetDir string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	return ip.extractTarReader(tar.NewReader(file), targetDir)
}

func (ip *InlineProcessor) extractTarReader(tarReader *tar.Reader, targetDir string) (string, error) {
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		path := filepath.Join(targetDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return "", err
			}

			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return "", err
			}

			_, err = io.Copy(file, tarReader)
			file.Close()

			if err != nil {
				return "", err
			}
		}
	}

	return ip.calculateDirectoryHash(targetDir)
}

func (ip *InlineProcessor) calculateDirectoryHash(dir string) (string, error) {
	hash := sha256.New()

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(dir, path)
		hash.Write([]byte(relPath))

		if !info.IsDir() {
			hash.Write([]byte(fmt.Sprintf("%d", info.ModTime().Unix())))
			hash.Write([]byte(fmt.Sprintf("%d", info.Size())))
		}

		return nil
	})

	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil))[:16], nil
}

func (ip *InlineProcessor) scanDirectory(ctx context.Context, dir string) ([]*FileInfo, *repocontextv1.RepositoryStats, error) {
	var files []*FileInfo
	stats := &repocontextv1.RepositoryStats{}

	log.Printf("scanDirectory: Starting scan of directory %s", dir)
	languageStats := make(map[string]*repocontextv1.LanguageStats)

	excludePatterns := []*regexp.Regexp{
		regexp.MustCompile(`\.git/`),
		regexp.MustCompile(`node_modules/`),
		regexp.MustCompile(`vendor/`),
		regexp.MustCompile(`\.DS_Store`),
		regexp.MustCompile(`\.(exe|dll|so|dylib|bin)$`),
		regexp.MustCompile(`\.(jpg|jpeg|png|gif|bmp|ico|svg)$`),
		regexp.MustCompile(`\.(pdf|doc|docx|xls|xlsx|ppt|pptx)$`),
		regexp.MustCompile(`\.(zip|tar|gz|rar|7z)$`),
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("scanDirectory: Walk error for path %s: %v", path, err)
			return err
		}

		if info.IsDir() {
			return nil
		}

		relPath, _ := filepath.Rel(dir, path)
		log.Printf("scanDirectory: Found file %s", relPath)

		// Check exclude patterns
		for _, pattern := range excludePatterns {
			if pattern.MatchString(relPath) {
				log.Printf("scanDirectory: Excluding %s (matches pattern %s)", relPath, pattern.String())
				return nil
			}
		}

		// Check if file is text
		isText, isBinary := ip.detectFileType(path)
		log.Printf("scanDirectory: File %s - isText: %v, isBinary: %v", relPath, isText, isBinary)
		if isBinary {
			log.Printf("scanDirectory: Skipping binary file %s", relPath)
			return nil
		}

		language := detectLanguage(relPath)
		lineCount := 0

		if isText {
			if count, err := ip.countLines(path); err == nil {
				lineCount = count
			}
		}

		fileInfo := &FileInfo{
			Path:         relPath,
			Size:         info.Size(),
			Language:     language,
			IsText:       isText,
			IsBinary:     isBinary,
			LineCount:    lineCount,
			LastModified: info.ModTime(),
		}

		files = append(files, fileInfo)
		log.Printf("scanDirectory: Added file %s to files list", relPath)

		// Update stats
		stats.TotalFiles++
		stats.TotalLines += int32(lineCount)
		stats.SizeBytes += info.Size()

		if langStats, exists := languageStats[language]; exists {
			langStats.FileCount++
			langStats.LineCount += int32(lineCount)
		} else {
			languageStats[language] = &repocontextv1.LanguageStats{
				Language:  language,
				FileCount: 1,
				LineCount: int32(lineCount),
			}
		}

		return nil
	})

	if err != nil {
		log.Printf("scanDirectory: Walk completed with error: %v", err)
		return nil, nil, err
	}

	log.Printf("scanDirectory: Walk completed successfully. Found %d files", len(files))

	// Convert language stats
	for _, langStat := range languageStats {
		stats.Languages = append(stats.Languages, langStat)
	}

	return files, stats, nil
}

func (ip *InlineProcessor) detectFileType(path string) (isText, isBinary bool) {
	// First check by file extension - common text file extensions
	ext := strings.ToLower(filepath.Ext(path))
	textExtensions := map[string]bool{
		".txt": true, ".md": true, ".json": true, ".js": true, ".ts": true, ".jsx": true, ".tsx": true,
		".py": true, ".go": true, ".java": true, ".c": true, ".cpp": true, ".h": true, ".hpp": true,
		".css": true, ".html": true, ".xml": true, ".yml": true, ".yaml": true, ".toml": true,
		".sh": true, ".bash": true, ".sql": true, ".php": true, ".rb": true, ".rs": true,
		".dockerfile": true, ".gitignore": true, ".gitattributes": true, ".env": true,
	}

	// Also check files without extension that are commonly text
	filename := filepath.Base(path)
	textFilenames := map[string]bool{
		"README": true, "LICENSE": true, "CHANGELOG": true, "Makefile": true,
		"Dockerfile": true, ".gitignore": true, ".dockerignore": true,
	}

	if textExtensions[ext] || textFilenames[filename] {
		return true, false
	}

	file, err := os.Open(path)
	if err != nil {
		log.Printf("detectFileType: Cannot open file %s: %v, treating as binary", path, err)
		return false, true
	}
	defer file.Close()

	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && err != io.EOF {
		log.Printf("detectFileType: Cannot read file %s: %v, treating as binary", path, err)
		return false, true
	}

	// Empty files are considered text
	if n == 0 {
		return true, false
	}

	// Check if valid UTF-8
	if utf8.Valid(buffer[:n]) {
		// Additional check: if file contains too many null bytes, it's likely binary
		nullCount := 0
		for i := 0; i < n; i++ {
			if buffer[i] == 0 {
				nullCount++
			}
		}

		// If more than 30% null bytes, consider it binary
		if float64(nullCount)/float64(n) > 0.3 {
			return false, true
		}

		return true, false
	}

	return false, true
}

func (ip *InlineProcessor) countLines(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
	}

	return lineCount, scanner.Err()
}

func (ip *InlineProcessor) updateJobStatus(ctx context.Context, job *IngestionJob) {
	job.Status.UpdatedAt = timestamppb.Now()
	job.UpdatedAt = time.Now()

	cachedStatus := &cache.CachedUploadStatus{
		UploadID:     job.ID,
		RepositoryID: job.RepositoryID,
		Status:       job.Status,
		Progress:     job.Progress,
		ErrorMessage: job.ErrorMessage,
		CreatedAt:    job.CreatedAt,
	}

	ip.cache.SetUploadStatus(ctx, job.TenantID, cachedStatus)
}

func (ip *InlineProcessor) GetIndexStatus(ctx context.Context, repoID string) (*repocontextv1.IngestionStatus, error) {
	// Implementation depends on how you store job status
	// This is a simplified version
	return &repocontextv1.IngestionStatus{
		State:     repocontextv1.IngestionStatus_STATE_READY,
		UpdatedAt: timestamppb.Now(),
	}, nil
}

func (ip *InlineProcessor) DeleteIndex(ctx context.Context, repoID string) error {
	// Delete from vector store
	className := toWeaviateClassName(repoID)
	if err := ip.vectorClient.DeleteCollection(ctx, className); err != nil {
		return fmt.Errorf("failed to delete vector collection: %w", err)
	}

	// Clean up work directory
	workPath := filepath.Join(ip.workDir, repoID)
	if err := os.RemoveAll(workPath); err != nil {
		return fmt.Errorf("failed to clean up work directory: %w", err)
	}

	return nil
}

// Helper functions

func extractRepositoryName(source *repocontextv1.RepositorySource) string {
	switch src := source.Source.(type) {
	case *repocontextv1.RepositorySource_GitUrl:
		// Extract repo name from Git URL
		parts := strings.Split(src.GitUrl, "/")
		if len(parts) > 0 {
			name := parts[len(parts)-1]
			return strings.TrimSuffix(name, ".git")
		}
		return "unknown"
	case *repocontextv1.RepositorySource_UploadedFilename:
		// Extract name from filename
		name := filepath.Base(src.UploadedFilename)
		return strings.TrimSuffix(name, filepath.Ext(name))
	default:
		return "unknown"
	}
}

func generateRepoKey(source *repocontextv1.RepositorySource) string {
	switch src := source.Source.(type) {
	case *repocontextv1.RepositorySource_GitUrl:
		if source.CommitSha != "" {
			return fmt.Sprintf("%s@%s", src.GitUrl, source.CommitSha)
		}
		return fmt.Sprintf("%s@%s", src.GitUrl, source.Ref)
	case *repocontextv1.RepositorySource_UploadedFilename:
		return src.UploadedFilename
	default:
		return "unknown"
	}
}

func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))

	languageMap := map[string]string{
		".go":   "go",
		".js":   "javascript",
		".ts":   "typescript",
		".py":   "python",
		".java": "java",
		".cpp":  "cpp",
		".c":    "c",
		".h":    "c",
		".cs":   "csharp",
		".rb":   "ruby",
		".php":  "php",
		".sh":   "shell",
		".rs":   "rust",
		".kt":   "kotlin",
		".swift": "swift",
		".scala": "scala",
		".r":    "r",
		".sql":  "sql",
		".html": "html",
		".css":  "css",
		".scss": "scss",
		".less": "less",
		".json": "json",
		".xml":  "xml",
		".yaml": "yaml",
		".yml":  "yaml",
		".toml": "toml",
		".md":   "markdown",
		".txt":  "text",
	}

	if lang, exists := languageMap[ext]; exists {
		return lang
	}

	return "unknown"
}