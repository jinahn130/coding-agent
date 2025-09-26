package query

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
)

type RipgrepClient struct {
	metrics    *observability.Metrics
	tracer     *observability.Tracer
	workDir    string
	maxMatches int
}

type RipgrepMatch struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber    int    `json:"line_number"`
		AbsoluteOffset int64 `json:"absolute_offset"`
		Submatches    []struct {
			Match struct {
				Text string `json:"text"`
			} `json:"match"`
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"submatches"`
	} `json:"data"`
}

func NewRipgrepClient(metrics *observability.Metrics, tracer *observability.Tracer, workDir string) *RipgrepClient {
	return &RipgrepClient{
		metrics:    metrics,
		tracer:     tracer,
		workDir:    workDir,
		maxMatches: 1000, // Prevent runaway searches
	}
}

func (r *RipgrepClient) SearchLexical(ctx context.Context, repoID, query string, limit int, filters map[string]interface{}) ([]*repocontextv1.CodeChunk, error) {
	ctx, span := r.tracer.StartSearch(ctx, query, "lexical")
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.BackendAttr("ripgrep"),
		observability.RepositoryAttr(repoID),
		observability.QueryAttr(query),
	)

	timer := observability.StartTimer()
	defer func() {
		r.metrics.RecordBackendLatency("ripgrep", timer.Duration())
	}()

	// Build ripgrep command
	args, err := r.buildRipgrepArgs(query, limit, filters)
	if err != nil {
		return nil, fmt.Errorf("failed to build ripgrep args: %w", err)
	}

	// Set working directory to repository path
	repoPath := filepath.Join(r.workDir, repoID)

	// Execute ripgrep
	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Dir = repoPath

	// Get output
	output, err := cmd.Output()
	if err != nil {
		// Ripgrep returns exit code 1 when no matches found, which is not an error
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			r.metrics.RecordSearchResults("lexical", 0)
			return nil, nil
		}
		return nil, fmt.Errorf("ripgrep execution failed: %w", err)
	}

	// Parse results
	chunks, err := r.parseRipgrepOutput(output, repoID, query)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ripgrep output: %w", err)
	}

	r.metrics.RecordSearchResults("lexical", len(chunks))

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(chunks)),
	)

	return chunks, nil
}

func (r *RipgrepClient) buildRipgrepArgs(query string, limit int, filters map[string]interface{}) ([]string, error) {
	args := []string{
		"--json",              // Output in JSON format
		"--line-number",       // Include line numbers
		"--column",            // Include column numbers
		"--context", "2",      // Include 2 lines of context before/after
		"--max-count", strconv.Itoa(r.maxMatches), // Limit matches per file
		"--smart-case",        // Smart case matching
		// Binary files are automatically skipped by ripgrep by default
	}

	// Add language filters
	if languages, ok := filters["languages"].([]string); ok && len(languages) > 0 {
		for _, lang := range languages {
			if rgType := mapLanguageToRipgrepType(lang); rgType != "" {
				args = append(args, "--type", rgType)
			}
		}
	}

	// Add file pattern filters
	if patterns, ok := filters["file_patterns"].([]string); ok && len(patterns) > 0 {
		for _, pattern := range patterns {
			args = append(args, "--glob", pattern)
		}
	}

	// Add path prefix filter
	if pathPrefix, ok := filters["path_prefix"].(string); ok && pathPrefix != "" {
		args = append(args, "--glob", pathPrefix+"*")
	}

	// Convert query to regex pattern
	pattern, err := r.queryToRegex(query)
	if err != nil {
		return nil, fmt.Errorf("failed to convert query to regex: %w", err)
	}

	args = append(args, pattern)

	return args, nil
}

func (r *RipgrepClient) queryToRegex(query string) (string, error) {
	// Split query into terms
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return "", fmt.Errorf("empty query")
	}

	var patterns []string

	for _, term := range terms {
		term = strings.ToLower(term)

		// Create multiple patterns for fuzzy matching
		var termPatterns []string

		// 1. Exact match (literal)
		exactPattern := "(?i)" + regexp.QuoteMeta(term)
		termPatterns = append(termPatterns, exactPattern)

		// 2. Partial word match (term contained in larger words)
		if len(term) >= 3 {
			partialPattern := "(?i)\\w*" + regexp.QuoteMeta(term) + "\\w*"
			termPatterns = append(termPatterns, partialPattern)
		}

		// 3. Fuzzy matching for common abbreviations and variations
		fuzzyPatterns := r.generateFuzzyPatterns(term)
		termPatterns = append(termPatterns, fuzzyPatterns...)

		// Combine all patterns for this term with OR logic
		if len(termPatterns) > 1 {
			patterns = append(patterns, "("+strings.Join(termPatterns, "|")+")")
		} else {
			patterns = append(patterns, termPatterns[0])
		}
	}

	// Combine all term patterns with OR logic
	if len(patterns) == 1 {
		return patterns[0], nil
	}

	return "(" + strings.Join(patterns, "|") + ")", nil
}

func (r *RipgrepClient) generateFuzzyPatterns(term string) []string {
	var patterns []string

	// Common tech abbreviations and expansions
	fuzzyMap := map[string][]string{
		"auth":           {"authentication", "authorization", "authorize", "authenticated", "authenticator"},
		"authentication": {"auth", "authenticator", "authenticate"},
		"authorization":  {"auth", "authorize", "authz"},
		"config":         {"configuration", "configure", "conf"},
		"configuration":  {"config", "conf"},
		"db":             {"database", "data_base"},
		"database":       {"db", "data_base"},
		"api":            {"endpoint", "service", "rest", "graphql"},
		"endpoint":       {"api", "route", "handler"},
		"handler":        {"handle", "controller", "processor"},
		"service":        {"svc", "server", "api"},
		"server":         {"srv", "service", "daemon"},
		"client":         {"cli", "consumer"},
		"response":       {"resp", "result", "reply"},
		"request":        {"req", "query", "input"},
		"error":          {"err", "exception", "failure"},
		"function":       {"func", "method", "procedure"},
		"method":         {"func", "function"},
		"variable":       {"var", "field", "property"},
		"parameter":      {"param", "arg", "argument"},
		"middleware":     {"middleware", "interceptor", "filter"},
		"route":          {"router", "routing", "path"},
		"controller":     {"ctrl", "handler", "processor"},
		"model":          {"schema", "entity", "data"},
		"view":           {"template", "render", "display"},
		"user":           {"users", "account", "profile"},
		"password":       {"pwd", "pass", "secret"},
		"token":          {"jwt", "bearer", "session"},
		"session":        {"sess", "cookie", "token"},
	}

	// Add fuzzy matches if term exists in map
	if expansions, exists := fuzzyMap[term]; exists {
		for _, expansion := range expansions {
			pattern := "(?i)" + regexp.QuoteMeta(expansion)
			patterns = append(patterns, pattern)
		}
	}

	// Also check if term is an expansion of something
	for key, expansions := range fuzzyMap {
		for _, expansion := range expansions {
			if expansion == term {
				pattern := "(?i)" + regexp.QuoteMeta(key)
				patterns = append(patterns, pattern)
				break
			}
		}
	}

	// Camel case fuzzy matching - if term could be part of camelCase
	if len(term) >= 3 {
		// Match camelCase variations: authHandler, AuthService, etc.
		camelPattern := "(?i)" + regexp.QuoteMeta(strings.Title(term)) + "[A-Z]\\w*"
		patterns = append(patterns, camelPattern)

		// Match snake_case variations: auth_handler, user_auth, etc.
		snakePattern := "(?i)\\w*_?" + regexp.QuoteMeta(term) + "_?\\w*"
		patterns = append(patterns, snakePattern)
	}

	return patterns
}

func (r *RipgrepClient) parseRipgrepOutput(output []byte, repoID, query string) ([]*repocontextv1.CodeChunk, error) {
	var chunks []*repocontextv1.CodeChunk
	var chunkMap = make(map[string]*repocontextv1.CodeChunk)

	// Parse JSON lines
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var match RipgrepMatch
		if err := json.Unmarshal([]byte(line), &match); err != nil {
			continue // Skip invalid JSON lines
		}

		// Only process match lines
		if match.Type != "match" {
			continue
		}

		chunk := r.convertMatchToChunk(match, repoID)
		if chunk == nil {
			continue
		}

		// Group matches by file and merge nearby matches
		key := fmt.Sprintf("%s:%d", chunk.FilePath, chunk.StartLine/10) // Group by file and line region
		if existingChunk, exists := chunkMap[key]; exists {
			// Merge chunks from the same file region
			existingChunk.Content += "\n" + chunk.Content
			if chunk.EndLine > existingChunk.EndLine {
				existingChunk.EndLine = chunk.EndLine
			}
			if chunk.StartLine < existingChunk.StartLine {
				existingChunk.StartLine = chunk.StartLine
			}
			// Update score (simple max for now)
			if chunk.Score > existingChunk.Score {
				existingChunk.Score = chunk.Score
			}
		} else {
			chunkMap[key] = chunk
		}
	}

	// Convert map to slice
	for _, chunk := range chunkMap {
		chunks = append(chunks, chunk)
	}

	// Sort by relevance score (descending)
	// For now, use a simple heuristic based on match quality
	for _, chunk := range chunks {
		chunk.Score = r.calculateRelevanceScore(chunk, query)
	}

	return chunks, nil
}

func (r *RipgrepClient) convertMatchToChunk(match RipgrepMatch, repoID string) *repocontextv1.CodeChunk {
	if match.Data.Path.Text == "" || match.Data.Lines.Text == "" {
		return nil
	}

	// Extract file path and clean it
	filePath := strings.TrimPrefix(match.Data.Path.Text, "./")

	// Detect language from file extension
	language := detectLanguageFromPath(filePath)

	chunk := &repocontextv1.CodeChunk{
		RepositoryId: repoID,
		FilePath:     filePath,
		StartLine:    int32(match.Data.LineNumber),
		EndLine:      int32(match.Data.LineNumber), // Will be updated when merging
		Content:      match.Data.Lines.Text,
		Language:     language,
		Source:       repocontextv1.SearchSource_SEARCH_SOURCE_LEXICAL,
		Score:        1.0, // Will be calculated later
	}

	return chunk
}

func (r *RipgrepClient) calculateRelevanceScore(chunk *repocontextv1.CodeChunk, query string) float32 {
	content := strings.ToLower(chunk.Content)
	queryLower := strings.ToLower(query)
	terms := strings.Fields(queryLower)

	score := float32(0.0)

	// Base score for having any matches
	hasMatch := false
	for _, term := range terms {
		if strings.Contains(content, term) {
			hasMatch = true

			// Count occurrences of this term
			count := strings.Count(content, term)
			score += float32(count) * 0.1

			// Bonus for exact word matches
			wordRegex := regexp.MustCompile(`\b` + regexp.QuoteMeta(term) + `\b`)
			if wordRegex.MatchString(content) {
				score += 0.2
			}
		}
	}

	if !hasMatch {
		return 0.0
	}

	// Bonus for shorter content (more focused matches)
	if len(chunk.Content) < 200 {
		score += 0.1
	}

	// Bonus for certain file types
	switch chunk.Language {
	case "go", "javascript", "typescript", "python", "java":
		score += 0.05
	}

	// Penalty for very long content
	if len(chunk.Content) > 1000 {
		score -= 0.1
	}

	// Normalize score to 0-1 range
	if score > 1.0 {
		score = 1.0
	}

	return score
}

// Helper functions

func mapLanguageToRipgrepType(language string) string {
	languageMap := map[string]string{
		"go":         "go",
		"javascript": "js",
		"typescript": "ts",
		"python":     "py",
		"java":       "java",
		"cpp":        "cpp",
		"c":          "c",
		"csharp":     "csharp",
		"ruby":       "ruby",
		"php":        "php",
		"shell":      "sh",
		"rust":       "rust",
		"kotlin":     "kotlin",
		"swift":      "swift",
		"scala":      "scala",
		"r":          "r",
		"sql":        "sql",
		"html":       "html",
		"css":        "css",
		"json":       "json",
		"xml":        "xml",
		"yaml":       "yaml",
		"markdown":   "md",
	}

	return languageMap[language]
}

func detectLanguageFromPath(path string) string {
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

func (r *RipgrepClient) HealthCheck(ctx context.Context) error {
	// Simple check: verify ripgrep is available
	cmd := exec.CommandContext(ctx, "rg", "--version")
	return cmd.Run()
}