package composer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"repo-context-service/internal/config"
	"repo-context-service/internal/observability"
	repocontextv1 "repo-context-service/proto/gen/repocontext/v1"
)

type DeepSeekClient struct {
	config     config.DeepSeekConfig
	httpClient *http.Client
	metrics    *observability.Metrics
	tracer     *observability.Tracer
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	Delta        Message `json:"delta"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type StreamResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
}

type CompositionResult struct {
	FullResponse string
	Citations    []*repocontextv1.Citation
	TokenCount   int
	Duration     time.Duration
}

func NewDeepSeekClient(cfg config.DeepSeekConfig, metrics *observability.Metrics, tracer *observability.Tracer) *DeepSeekClient {
	return &DeepSeekClient{
		config: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		metrics: metrics,
		tracer:  tracer,
	}
}

func (d *DeepSeekClient) ComposeAnswer(ctx context.Context, query string, chunks []*repocontextv1.CodeChunk) (*CompositionResult, error) {
	ctx, span := d.tracer.StartLLMCall(ctx, d.config.Model)
	defer span.End()

	observability.SetSpanAttributes(span,
		observability.ModelAttr(d.config.Model),
		observability.QueryAttr(query),
		observability.ResultCountAttr(len(chunks)),
	)

	timer := observability.StartTimer()
	defer func() {
		d.metrics.RecordBackendLatency("deepseek", timer.Duration())
	}()

	// Build prompt
	systemPrompt := buildSystemPrompt()
	userPrompt := buildUserPrompt(query, chunks)

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	// Create request
	req := ChatRequest{
		Model:       d.config.Model,
		Messages:    messages,
		Temperature: d.config.Temperature,
		MaxTokens:   d.config.MaxTokens,
		Stream:      false, // For now, use non-streaming
	}

	// Make API call
	response, err := d.makeAPICall(ctx, req)
	if err != nil {
		d.metrics.RecordLLMRequest(d.config.Model, "error")
		return nil, fmt.Errorf("DeepSeek API call failed: %w", err)
	}

	d.metrics.RecordLLMRequest(d.config.Model, "success")

	// Extract response
	if len(response.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	fullResponse := response.Choices[0].Message.Content
	citations := extractCitations(fullResponse, chunks)

	result := &CompositionResult{
		FullResponse: fullResponse,
		Citations:    citations,
		TokenCount:   response.Usage.TotalTokens,
		Duration:     timer.Duration(),
	}

	observability.SetSpanAttributes(span,
		observability.ResultCountAttr(len(citations)),
	)

	return result, nil
}

func (d *DeepSeekClient) ComposeAnswerStream(ctx context.Context, query string, chunks []*repocontextv1.CodeChunk, callback func(string) error) (*CompositionResult, error) {
	ctx, span := d.tracer.StartLLMCall(ctx, d.config.Model)
	defer span.End()

	if !d.config.StreamTokens {
		// Fallback to non-streaming
		result, err := d.ComposeAnswer(ctx, query, chunks)
		if err != nil {
			return nil, err
		}
		// Send full response at once
		if err := callback(result.FullResponse); err != nil {
			return nil, err
		}
		return result, nil
	}

	timer := observability.StartTimer()
	defer func() {
		d.metrics.RecordBackendLatency("deepseek", timer.Duration())
	}()

	// Build prompt
	systemPrompt := buildSystemPrompt()
	userPrompt := buildUserPrompt(query, chunks)

	messages := []Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	// Create streaming request
	req := ChatRequest{
		Model:       d.config.Model,
		Messages:    messages,
		Temperature: d.config.Temperature,
		MaxTokens:   d.config.MaxTokens,
		Stream:      true,
	}

	// Make streaming API call
	fullResponse, tokenCount, err := d.makeStreamingAPICall(ctx, req, callback)
	if err != nil {
		d.metrics.RecordLLMRequest(d.config.Model, "error")
		return nil, fmt.Errorf("DeepSeek streaming API call failed: %w", err)
	}

	d.metrics.RecordLLMRequest(d.config.Model, "success")

	citations := extractCitations(fullResponse, chunks)

	result := &CompositionResult{
		FullResponse: fullResponse,
		Citations:    citations,
		TokenCount:   tokenCount,
		Duration:     timer.Duration(),
	}

	return result, nil
}

func (d *DeepSeekClient) makeAPICall(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	requestBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/chat/completions", strings.NewReader(string(requestBody)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.config.APIKey)

	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var response ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &response, nil
}

func (d *DeepSeekClient) makeStreamingAPICall(ctx context.Context, req ChatRequest, callback func(string) error) (string, int, error) {
	requestBody, err := json.Marshal(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.deepseek.com/chat/completions", strings.NewReader(string(requestBody)))
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+d.config.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := d.httpClient.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse server-sent events
	var fullResponse strings.Builder
	tokenCount := 0

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		// Parse data: lines
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")

			// Check for end of stream
			if data == "[DONE]" {
				break
			}

			var streamResp StreamResponse
			if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
				continue // Skip invalid JSON
			}

			if len(streamResp.Choices) > 0 {
				delta := streamResp.Choices[0].Delta.Content
				if delta != "" {
					fullResponse.WriteString(delta)
					tokenCount++

					// Send token to callback
					if err := callback(delta); err != nil {
						return "", 0, fmt.Errorf("callback error: %w", err)
					}
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", 0, fmt.Errorf("failed to read stream: %w", err)
	}

	return fullResponse.String(), tokenCount, nil
}

// Helper functions

func buildSystemPrompt() string {
	return `You are an expert code assistant that helps developers understand repositories by analyzing code chunks and answering questions.

Your task is to provide helpful, accurate answers based on the provided code context. Follow these guidelines:

1. **Be concise but comprehensive** - Provide direct answers without unnecessary verbosity
2. **Reference specific code** - When mentioning code elements, reference the file and line numbers
3. **Explain context** - Help the user understand not just what the code does, but why
4. **Use proper formatting** - Use markdown for code blocks, lists, and emphasis
5. **Be honest about limitations** - If the context doesn't contain enough information, say so
6. **Focus on the question** - Stay relevant to what the user is asking

When referencing code:
- Use the format: ` + "`" + `file_path:line_number` + "`" + ` for specific references
- Include relevant code snippets when helpful
- Explain the purpose and context of code elements

Remember: You can only answer based on the provided code chunks. Don't make assumptions about code that isn't shown.`
}

func buildUserPrompt(query string, chunks []*repocontextv1.CodeChunk) string {
	var prompt strings.Builder

	prompt.WriteString(fmt.Sprintf("**Question:** %s\n\n", query))
	prompt.WriteString("**Code Context:**\n\n")

	for i, chunk := range chunks {
		prompt.WriteString(fmt.Sprintf("**File %d:** `%s` (lines %d-%d)\n", i+1, chunk.FilePath, chunk.StartLine, chunk.EndLine))
		if chunk.Language != "" && chunk.Language != "unknown" {
			prompt.WriteString(fmt.Sprintf("```%s\n", chunk.Language))
		} else {
			prompt.WriteString("```\n")
		}
		prompt.WriteString(chunk.Content)
		prompt.WriteString("\n```\n\n")
	}

	prompt.WriteString("Please analyze this code context and answer the question. Reference specific files and line numbers when relevant.")

	return prompt.String()
}

func extractCitations(response string, chunks []*repocontextv1.CodeChunk) []*repocontextv1.Citation {
	var citations []*repocontextv1.Citation

	// Look for file references in the response
	for _, chunk := range chunks {
		if strings.Contains(response, chunk.FilePath) {
			// Extract a short excerpt from the chunk for the citation
			excerpt := chunk.Content
			if len(excerpt) > 100 {
				lines := strings.Split(excerpt, "\n")
				if len(lines) > 2 {
					excerpt = strings.Join(lines[:2], "\n") + "..."
				} else {
					excerpt = excerpt[:100] + "..."
				}
			}

			citation := &repocontextv1.Citation{
				FilePath:   chunk.FilePath,
				LineNumber: chunk.StartLine,
				Excerpt:    excerpt,
			}

			citations = append(citations, citation)
		}
	}

	return citations
}

