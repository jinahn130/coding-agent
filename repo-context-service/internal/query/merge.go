package query

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
)

type MergedResults struct {
	Chunks  []*repocontextv1.CodeChunk
	Timings *repocontextv1.SearchTimings
	Stats   *repocontextv1.SearchStats
}

type SearchResults struct {
	LexicalChunks  []*repocontextv1.CodeChunk
	SemanticChunks []*repocontextv1.CodeChunk
	LexicalTime    time.Duration
	SemanticTime   time.Duration
	CacheHit       bool
}

type ResultMerger struct {
	maxResults int
}

func NewResultMerger(maxResults int) *ResultMerger {
	return &ResultMerger{
		maxResults: maxResults,
	}
}

func (rm *ResultMerger) MergeAndRank(results *SearchResults) *MergedResults {
	startTime := time.Now()

	// Normalize scores for each backend
	lexicalNormalized := rm.normalizeScores(results.LexicalChunks)
	semanticNormalized := rm.normalizeScores(results.SemanticChunks)

	// Merge results
	merged := rm.mergeResults(lexicalNormalized, semanticNormalized)

	// Deduplicate and rank
	final := rm.deduplicateAndRank(merged)

	// Truncate to max results
	if len(final) > rm.maxResults {
		final = final[:rm.maxResults]
	}

	// Calculate merge time
	mergeTime := time.Since(startTime)

	return &MergedResults{
		Chunks: final,
		Timings: &repocontextv1.SearchTimings{
			LexicalMs:     int32(results.LexicalTime.Milliseconds()),
			SemanticMs:    int32(results.SemanticTime.Milliseconds()),
			MergeMs:       int32(mergeTime.Milliseconds()),
			CompositionMs: 0, // Will be set later
			CacheHit:      results.CacheHit,
		},
		Stats: &repocontextv1.SearchStats{
			LexicalCandidates:  int32(len(results.LexicalChunks)),
			SemanticCandidates: int32(len(results.SemanticChunks)),
			MergedResults:      int32(len(final)),
			ResultsTruncated:   len(final) == rm.maxResults,
		},
	}
}

func (rm *ResultMerger) normalizeScores(chunks []*repocontextv1.CodeChunk) []*repocontextv1.CodeChunk {
	if len(chunks) == 0 {
		return chunks
	}

	// Calculate mean and standard deviation for z-score normalization
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

	// Avoid division by zero
	if stdDev == 0 {
		stdDev = 1
	}

	// Normalize using z-score and map to 0-1 range
	normalized := make([]*repocontextv1.CodeChunk, len(chunks))
	for i, chunk := range chunks {
		// Copy chunk
		normalized[i] = &repocontextv1.CodeChunk{
			RepositoryId: chunk.RepositoryId,
			FilePath:     chunk.FilePath,
			StartLine:    chunk.StartLine,
			EndLine:      chunk.EndLine,
			Content:      chunk.Content,
			Language:     chunk.Language,
			Symbol:       chunk.Symbol,
			Source:       chunk.Source,
		}

		// Z-score normalization
		zScore := (float64(chunk.Score) - mean) / stdDev

		// Map to 0-1 range using sigmoid
		normalized[i].Score = float32(1.0 / (1.0 + math.Exp(-zScore)))
	}

	return normalized
}

func (rm *ResultMerger) mergeResults(lexical, semantic []*repocontextv1.CodeChunk) []*repocontextv1.CodeChunk {
	var merged []*repocontextv1.CodeChunk

	// Add lexical results
	for _, chunk := range lexical {
		chunk.Source = repocontextv1.SearchSource_SEARCH_SOURCE_LEXICAL
		merged = append(merged, chunk)
	}

	// Add semantic results
	for _, chunk := range semantic {
		chunk.Source = repocontextv1.SearchSource_SEARCH_SOURCE_SEMANTIC
		merged = append(merged, chunk)
	}

	return merged
}

func (rm *ResultMerger) deduplicateAndRank(chunks []*repocontextv1.CodeChunk) []*repocontextv1.CodeChunk {
	if len(chunks) == 0 {
		return chunks
	}

	// Group chunks by file
	fileGroups := make(map[string][]*repocontextv1.CodeChunk)
	for _, chunk := range chunks {
		fileGroups[chunk.FilePath] = append(fileGroups[chunk.FilePath], chunk)
	}

	var final []*repocontextv1.CodeChunk

	// Process each file group
	for _, fileChunks := range fileGroups {
		deduplicated := rm.deduplicateFileChunks(fileChunks)

		// Apply file-level boosting
		for _, chunk := range deduplicated {
			chunk.Score = rm.applyBoosts(chunk, fileChunks)
		}

		final = append(final, deduplicated...)
	}

	// Sort by score (descending)
	sort.Slice(final, func(i, j int) bool {
		return final[i].Score > final[j].Score
	})

	return final
}

func (rm *ResultMerger) deduplicateFileChunks(chunks []*repocontextv1.CodeChunk) []*repocontextv1.CodeChunk {
	if len(chunks) <= 1 {
		return chunks
	}

	// Sort by line number
	sort.Slice(chunks, func(i, j int) bool {
		return chunks[i].StartLine < chunks[j].StartLine
	})

	var deduplicated []*repocontextv1.CodeChunk

	for _, chunk := range chunks {
		// Check for overlap with previous chunks
		overlap := false
		for j := len(deduplicated) - 1; j >= 0; j-- {
			if rm.hasOverlap(deduplicated[j], chunk) {
				// Merge overlapping chunks
				merged := rm.mergeOverlappingChunks(deduplicated[j], chunk)
				deduplicated[j] = merged
				overlap = true
				break
			}
		}

		if !overlap {
			deduplicated = append(deduplicated, rm.copyChunk(chunk))
		}
	}

	return deduplicated
}

func (rm *ResultMerger) hasOverlap(chunk1, chunk2 *repocontextv1.CodeChunk) bool {
	if chunk1.FilePath != chunk2.FilePath {
		return false
	}

	// Check for line overlap or proximity (within 5 lines)
	return (chunk1.EndLine >= chunk2.StartLine-5) && (chunk1.StartLine <= chunk2.EndLine+5)
}

func (rm *ResultMerger) mergeOverlappingChunks(chunk1, chunk2 *repocontextv1.CodeChunk) *repocontextv1.CodeChunk {
	merged := rm.copyChunk(chunk1)

	// Expand line range
	if chunk2.StartLine < merged.StartLine {
		merged.StartLine = chunk2.StartLine
	}
	if chunk2.EndLine > merged.EndLine {
		merged.EndLine = chunk2.EndLine
	}

	// Combine content (avoid duplication)
	if !strings.Contains(merged.Content, chunk2.Content) {
		merged.Content = rm.combineContent(merged.Content, chunk2.Content)
	}

	// Use higher score
	if chunk2.Score > merged.Score {
		merged.Score = chunk2.Score
	}

	// Mark as merged source
	merged.Source = repocontextv1.SearchSource_SEARCH_SOURCE_MERGED

	return merged
}

func (rm *ResultMerger) combineContent(content1, content2 string) string {
	// Simple combination - in practice, you might want more sophisticated merging
	lines1 := strings.Split(content1, "\n")
	lines2 := strings.Split(content2, "\n")

	// Use the longer content
	if len(lines2) > len(lines1) {
		return content2
	}
	return content1
}

func (rm *ResultMerger) applyBoosts(chunk *repocontextv1.CodeChunk, allFileChunks []*repocontextv1.CodeChunk) float32 {
	score := chunk.Score

	// File boost: If the same file appears in both backends
	hasLexical := false
	hasSemantic := false
	for _, c := range allFileChunks {
		if c.Source == repocontextv1.SearchSource_SEARCH_SOURCE_LEXICAL {
			hasLexical = true
		}
		if c.Source == repocontextv1.SearchSource_SEARCH_SOURCE_SEMANTIC {
			hasSemantic = true
		}
	}

	if hasLexical && hasSemantic {
		score += 0.15 // File appears in both backends
	}

	// Shorter span boost
	lineSpan := chunk.EndLine - chunk.StartLine + 1
	if lineSpan <= 10 {
		score += 0.05 // Prefer focused, shorter chunks
	} else if lineSpan > 50 {
		score -= 0.02 // Penalize very long chunks
	}

	// Language boost for popular languages
	switch chunk.Language {
	case "go", "javascript", "typescript", "python", "java":
		score += 0.02
	}

	// File type boost
	if strings.HasSuffix(chunk.FilePath, "_test.go") ||
	   strings.HasSuffix(chunk.FilePath, ".test.js") ||
	   strings.Contains(chunk.FilePath, "test/") {
		score -= 0.01 // Slightly penalize test files
	}

	if strings.Contains(chunk.FilePath, "main.") ||
	   strings.Contains(chunk.FilePath, "index.") ||
	   strings.Contains(chunk.FilePath, "app.") {
		score += 0.02 // Boost main/entry files
	}

	// Content quality boost
	contentLines := strings.Split(chunk.Content, "\n")
	nonEmptyLines := 0
	for _, line := range contentLines {
		if strings.TrimSpace(line) != "" {
			nonEmptyLines++
		}
	}

	if nonEmptyLines > 0 {
		density := float32(nonEmptyLines) / float32(len(contentLines))
		if density > 0.7 {
			score += 0.03 // Boost for dense, non-empty content
		}
	}

	// Ensure score stays in valid range
	if score > 1.0 {
		score = 1.0
	}
	if score < 0.0 {
		score = 0.0
	}

	return score
}

func (rm *ResultMerger) copyChunk(chunk *repocontextv1.CodeChunk) *repocontextv1.CodeChunk {
	return &repocontextv1.CodeChunk{
		RepositoryId: chunk.RepositoryId,
		FilePath:     chunk.FilePath,
		StartLine:    chunk.StartLine,
		EndLine:      chunk.EndLine,
		Content:      chunk.Content,
		Score:        chunk.Score,
		Source:       chunk.Source,
		Language:     chunk.Language,
		Symbol:       chunk.Symbol,
	}
}

// TruncateContent truncates chunk content to a reasonable size for display
func (rm *ResultMerger) TruncateContent(chunks []*repocontextv1.CodeChunk, maxLength int) {
	for _, chunk := range chunks {
		if len(chunk.Content) > maxLength {
			// Try to truncate at line boundaries
			lines := strings.Split(chunk.Content, "\n")
			var truncated []string
			currentLength := 0

			for _, line := range lines {
				if currentLength+len(line) > maxLength-3 { // Reserve space for "..."
					break
				}
				truncated = append(truncated, line)
				currentLength += len(line) + 1 // +1 for newline
			}

			chunk.Content = strings.Join(truncated, "\n") + "..."
		}
	}
}

// RedactSecrets removes likely secrets from chunk content
func (rm *ResultMerger) RedactSecrets(chunks []*repocontextv1.CodeChunk) {
	// Patterns that might indicate secrets
	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(password|pwd|secret|key|token|auth)\s*[:=]\s*["']([^"']{8,})["']`),
		regexp.MustCompile(`(?i)(api_key|apikey|access_key)\s*[:=]\s*["']([^"']{8,})["']`),
		regexp.MustCompile(`(?i)(private_key|privkey)\s*[:=]\s*["']([^"']{20,})["']`),
		regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`), // Base64-like strings
	}

	for _, chunk := range chunks {
		content := chunk.Content
		for _, pattern := range secretPatterns {
			content = pattern.ReplaceAllStringFunc(content, func(match string) string {
				// Replace with redacted version
				return strings.Replace(match, pattern.FindStringSubmatch(match)[2], "[REDACTED]", 1)
			})
		}
		chunk.Content = content
	}
}

