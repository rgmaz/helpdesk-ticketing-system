package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ChatRequest is one provider-agnostic completion request.
type ChatRequest struct {
	System      string
	User        string
	MaxTokens   int
	Temperature float64
}

// ChatResponse is the provider's completion plus token accounting.
type ChatResponse struct {
	Content          string
	Model            string
	PromptTokens     int
	CompletionTokens int
}

// Provider is the seam that keeps the rest of the code independent of a
// particular vendor. Tests substitute a fake; production uses an
// OpenAI-compatible endpoint.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Name() string
}

// OpenAIProvider talks to any endpoint that implements the OpenAI chat
// completions shape (/chat/completions), including Azure OpenAI, Groq, Together
// and local servers, by changing the base URL.
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewOpenAIProvider builds the HTTP provider.
func NewOpenAIProvider(cfg Config) *OpenAIProvider {
	return &OpenAIProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		client:  &http.Client{Timeout: cfg.Timeout},
	}
}

// Name reports the configured model, used in suggestions and metrics.
func (p *OpenAIProvider) Name() string { return p.model }

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model          string          `json:"model"`
	Messages       []openAIMessage `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

type openAIResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// httpError carries a status code so the retry wrapper can tell a transient 429
// or 503 apart from a permanent 400.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("ai provider returned status %d", e.Status)
	}
	return fmt.Sprintf("ai provider returned status %d: %s", e.Status, e.Body)
}

// Chat performs one completion. The caller's context carries the deadline; the
// HTTP client timeout is a second, outer bound.
func (p *OpenAIProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := openAIRequest{
		Model: p.model,
		Messages: []openAIMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		ResponseFormat: &struct {
			Type string `json:"type"`
		}{Type: "json_object"},
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return ChatResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	res, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer res.Body.Close()

	// Read a bounded body: a provider returning an HTML error page must not be
	// able to balloon memory or leak into the log.
	limited := io.LimitReader(res.Body, 1<<20)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return ChatResponse{}, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return ChatResponse{}, &httpError{
			Status: res.StatusCode,
			Body:   strings.TrimSpace(string(payload)),
		}
	}

	var decoded openAIResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ChatResponse{}, fmt.Errorf("decode ai response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return ChatResponse{}, errors.New("ai response contained no choices")
	}

	return ChatResponse{
		Content:          decoded.Choices[0].Message.Content,
		Model:            firstNonEmpty(decoded.Model, p.model),
		PromptTokens:     decoded.Usage.PromptTokens,
		CompletionTokens: decoded.Usage.CompletionTokens,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ErrUnavailable means the provider is temporarily unusable. Callers degrade to
// no suggestions rather than surfacing an error to the requester.
var ErrUnavailable = errors.New("ai provider is unavailable")

// gateway wraps a Provider with bounded retries and a small circuit breaker. It
// exists so the service layer does not have to reason about transient provider
// failures or hammer a service that is already down.
type gateway struct {
	provider Provider
	attempts int
	now      func() time.Time

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func newGateway(p Provider, attempts int) *gateway {
	if attempts < 1 {
		attempts = 1
	}
	return &gateway{provider: p, attempts: attempts, now: time.Now}
}

// Chat runs the request, retrying only failures that are plausibly transient.
func (g *gateway) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if g.provider == nil {
		return ChatResponse{}, ErrUnavailable
	}

	g.mu.Lock()
	open := g.now().Before(g.openUntil)
	g.mu.Unlock()
	if open {
		return ChatResponse{}, ErrUnavailable
	}

	var lastErr error
	for attempt := 0; attempt < g.attempts; attempt++ {
		resp, err := g.provider.Chat(ctx, req)
		if err == nil {
			g.mu.Lock()
			g.failures = 0
			g.mu.Unlock()
			return resp, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			break
		}
		if !retryable(err) {
			break
		}
		if attempt < g.attempts-1 {
			backoff := time.Duration(250*(1<<attempt)) * time.Millisecond
			select {
			case <-ctx.Done():
				return ChatResponse{}, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}

	g.recordFailure(lastErr)
	return ChatResponse{}, lastErr
}

func (g *gateway) recordFailure(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	if g.failures >= 5 {
		g.openUntil = g.now().Add(60 * time.Second)
		g.failures = 0
		slog.Warn("ai provider circuit opened for 60s", "error", err)
	}
}

// retryable classifies an error: rate limits and 5xx are worth another attempt,
// while a 4xx caused by the request itself is not.
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	// Transport errors (DNS, connection reset, timeout) are retryable; context
	// cancellation is handled by the caller before this is reached.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
