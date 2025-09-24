package ingest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"repo-context-service/internal/observability"
)

func (ip *InlineProcessor) ChunkFiles(ctx context.Context, extractResult *ExtractResult, options *ChunkOptions) ([]*FileChunk, error) {
	ctx, span := ip.tracer.StartIngestion(ctx, "", "chunk_files")
	defer span.End()

	var allChunks []*FileChunk
	excludeRegexes := compilePatterns(options.ExcludePatterns)
	includeRegexes := compilePatterns(options.IncludePatterns)

	log.Printf("ChunkFiles: Starting with %d files", len(extractResult.Files))
	if len(extractResult.Files) == 0 {
		log.Printf("ChunkFiles: No files found in extractResult!")
		return allChunks, nil
	}

	for _, fileInfo := range extractResult.Files {
		log.Printf("ChunkFiles: Processing file %s (IsText: %v, IsBinary: %v, Size: %d)",
			fileInfo.Path, fileInfo.IsText, fileInfo.IsBinary, fileInfo.Size)
		// Skip if file matches exclude patterns
		if matchesPatterns(fileInfo.Path, excludeRegexes) {
			log.Printf("ChunkFiles: Skipping %s (matches exclude pattern)", fileInfo.Path)
			continue
		}

		// Skip if include patterns are specified and file doesn't match
		if len(includeRegexes) > 0 && !matchesPatterns(fileInfo.Path, includeRegexes) {
			log.Printf("ChunkFiles: Skipping %s (doesn't match include pattern)", fileInfo.Path)
			continue
		}

		// Skip if file is too large
		if options.MaxFileSize > 0 && fileInfo.Size > options.MaxFileSize {
			log.Printf("ChunkFiles: Skipping %s (too large: %d > %d)", fileInfo.Path, fileInfo.Size, options.MaxFileSize)
			continue
		}

		// Skip binary files
		if fileInfo.IsBinary || !fileInfo.IsText {
			log.Printf("ChunkFiles: Skipping %s (binary or not text: IsBinary=%v, IsText=%v)",
				fileInfo.Path, fileInfo.IsBinary, fileInfo.IsText)
			continue
		}

		filePath := filepath.Join(extractResult.RepositoryPath, fileInfo.Path)
		chunks, err := ip.chunkFile(ctx, filePath, fileInfo, options)
		if err != nil {
			// Log error but continue with other files
			log.Printf("ChunkFiles: Error chunking file %s: %v", fileInfo.Path, err)
			observability.RecordError(span, err, fmt.Sprintf("failed to chunk file %s", fileInfo.Path))
			continue
		}

		log.Printf("ChunkFiles: Successfully created %d chunks for file %s", len(chunks), fileInfo.Path)
		allChunks = append(allChunks, chunks...)
	}

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(allChunks)),
	)

	log.Printf("ChunkFiles: Completed chunking. Total chunks created: %d", len(allChunks))
	return allChunks, nil
}

func (ip *InlineProcessor) chunkFile(ctx context.Context, filePath string, fileInfo *FileInfo, options *ChunkOptions) ([]*FileChunk, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file %s: %w", filePath, err)
	}
	defer file.Close()

	var chunks []*FileChunk
	scanner := bufio.NewScanner(file)

	var lines []string
	lineNumber := 1

	// Read all lines first
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
	}

	// Create chunks using sliding window
	chunkSize := options.ChunkSize
	overlap := options.ChunkOverlap

	for i := 0; i < len(lines); i += chunkSize - overlap {
		endIdx := i + chunkSize
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		// Skip chunks that are too small (less than 10% of chunk size)
		if endIdx-i < chunkSize/10 && i > 0 {
			break
		}

		chunkLines := lines[i:endIdx]
		content := strings.Join(chunkLines, "\n")

		// Skip empty or whitespace-only chunks
		if strings.TrimSpace(content) == "" {
			continue
		}

		// Create chunk
		chunk := &FileChunk{
			ID:           generateChunkID(fileInfo.Path, i+lineNumber, endIdx+lineNumber-1),
			RepositoryID: "", // Will be set by caller
			FilePath:     fileInfo.Path,
			StartLine:    i + lineNumber,
			EndLine:      endIdx + lineNumber - 1,
			Content:      content,
			Language:     fileInfo.Language,
			Size:         len(content),
			Hash:         hashContent(content),
		}

		chunks = append(chunks, chunk)

		// If we've reached the end, break
		if endIdx >= len(lines) {
			break
		}
	}

	return chunks, nil
}

func (ip *InlineProcessor) GenerateEmbeddings(ctx context.Context, chunks []*FileChunk) ([]*EmbeddedChunk, error) {
	ctx, span := ip.tracer.StartIngestion(ctx, "", "generate_embeddings")
	defer span.End()

	if len(chunks) == 0 {
		return nil, nil
	}

	// Set repository ID for all chunks
	for i := range chunks {
		if chunks[i].RepositoryID == "" {
			// Extract repo ID from context or set from somewhere
			chunks[i].RepositoryID = "repo-id" // This should be passed in properly
		}
	}

	// Prepare texts for embedding
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		// Combine file path and content for better context
		texts[i] = fmt.Sprintf("File: %s\nLanguage: %s\nContent:\n%s",
			chunk.FilePath, chunk.Language, chunk.Content)
	}

	timer := observability.StartTimer()
	embeddingModel := ip.embeddingClient.GetDefaultModel()
	embeddings, err := ip.embeddingClient.GenerateEmbeddings(ctx, texts, embeddingModel)
	if err != nil {
		ip.metrics.RecordEmbeddingRequest(embeddingModel, "error")
		return nil, fmt.Errorf("failed to generate embeddings: %w", err)
	}
	ip.metrics.RecordEmbeddingRequest(embeddingModel, "success")
	ip.metrics.RecordBackendLatency("openai", timer.Duration())

	if len(embeddings) != len(chunks) {
		return nil, fmt.Errorf("embedding count mismatch: got %d, expected %d", len(embeddings), len(chunks))
	}

	// Create embedded chunks
	embeddedChunks := make([]*EmbeddedChunk, len(chunks))
	for i, chunk := range chunks {
		embeddedChunks[i] = &EmbeddedChunk{
			FileChunk: chunk,
			Embedding: embeddings[i],
			Model:     "text-embedding-3-small",
			CreatedAt: time.Now(),
		}
	}

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(embeddedChunks)),
		observability.ModelAttr("text-embedding-3-small"),
	)

	return embeddedChunks, nil
}

func (ip *InlineProcessor) IndexEmbeddings(ctx context.Context, repoID string, chunks []*EmbeddedChunk) error {
	ctx, span := ip.tracer.StartIngestion(ctx, repoID, "index_embeddings")
	defer span.End()

	if len(chunks) == 0 {
		return nil
	}

	// Create collection if it doesn't exist
	dimensions := len(chunks[0].Embedding)
	// Convert repoID to valid Weaviate class name (PascalCase, no hyphens)
	className := toWeaviateClassName(repoID)
	if err := ip.vectorClient.CreateCollection(ctx, className, dimensions); err != nil {
		return fmt.Errorf("failed to create collection: %w", err)
	}

	// Convert to vectors
	vectors := make([]*Vector, len(chunks))
	for i, chunk := range chunks {
		vectors[i] = &Vector{
			ID:     chunk.ID,
			Vector: chunk.Embedding,
			Metadata: map[string]interface{}{
				"repository_id": chunk.RepositoryID,
				"file_path":     chunk.FilePath,
				"start_line":    chunk.StartLine,
				"end_line":      chunk.EndLine,
				"language":      chunk.Language,
				"size":          chunk.Size,
				"created_at":    chunk.CreatedAt.Unix(),
			},
		}
	}

	// Batch upsert (process in batches of 100)
	batchSize := 100
	for i := 0; i < len(vectors); i += batchSize {
		end := i + batchSize
		if end > len(vectors) {
			end = len(vectors)
		}

		batch := vectors[i:end]
		timer := observability.StartTimer()
		if err := ip.vectorClient.UpsertVectors(ctx, className, batch); err != nil {
			return fmt.Errorf("failed to upsert vectors batch %d-%d: %w", i, end, err)
		}
		ip.metrics.RecordBackendLatency("weaviate", timer.Duration())
	}

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(vectors)),
		observability.RepositoryAttr(repoID),
	)

	return nil
}

// Helper functions

// toWeaviateClassName converts a repository ID to a valid Weaviate class name
// Weaviate class names must be PascalCase and contain no hyphens or special characters
func toWeaviateClassName(repoID string) string {
	return "Repo" + strings.ReplaceAll(strings.TrimPrefix(repoID, "repo-"), "-", "")
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	var regexes []*regexp.Regexp
	for _, pattern := range patterns {
		if regex, err := regexp.Compile(pattern); err == nil {
			regexes = append(regexes, regex)
		}
	}
	return regexes
}

func matchesPatterns(path string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(path) {
			return true
		}
	}
	return false
}

func generateChunkID(filePath string, startLine, endLine int) string {
	content := fmt.Sprintf("%s:%d-%d", filePath, startLine, endLine)
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash)[:16]
}

func hashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash)[:16]
}

// Language-specific chunking strategies (future enhancement)

type ChunkingStrategy interface {
	ChunkContent(content string, options *ChunkOptions) []ChunkBoundary
}

type ChunkBoundary struct {
	StartLine int
	EndLine   int
	Type      string // "function", "class", "section", etc.
	Name      string // function/class name if applicable
}

type LineBasedStrategy struct{}

func (s *LineBasedStrategy) ChunkContent(content string, options *ChunkOptions) []ChunkBoundary {
	lines := strings.Split(content, "\n")
	var boundaries []ChunkBoundary

	chunkSize := options.ChunkSize
	overlap := options.ChunkOverlap

	for i := 0; i < len(lines); i += chunkSize - overlap {
		endIdx := i + chunkSize
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		boundaries = append(boundaries, ChunkBoundary{
			StartLine: i + 1,
			EndLine:   endIdx,
			Type:      "section",
		})

		if endIdx >= len(lines) {
			break
		}
	}

	return boundaries
}

// Future: Language-specific strategies
// type GoChunkingStrategy struct{}
// type JavaScriptChunkingStrategy struct{}
// type PythonChunkingStrategy struct{}

func getChunkingStrategy(language string) ChunkingStrategy {
	// For now, use line-based strategy for all languages
	// In the future, implement language-specific strategies
	return &LineBasedStrategy{}
}