package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
	"github.com/ryan/ticketing-system/backend/internal/service"
	"github.com/ryan/ticketing-system/backend/internal/store"
)

var (
	// ErrDisabled is returned when the feature flag is off. The API maps it to
	// 503 so every client behaves the same way without checking configuration.
	ErrDisabled = errors.New("ai assistant is disabled")
	// ErrRateLimited is returned when a caller exceeds the preflight allowance.
	ErrRateLimited = errors.New("ai assistant rate limit reached")
)

// Service is the advisory triage engine. It holds the provider, the job worker
// and the small amount of process-local state (rate limits, a short-lived
// preflight cache and the spend meter). Persistent state lives in store.AIRepo.
type Service struct {
	store *store.Store
	hub   *notify.Hub
	cfg   Config
	gw    *gateway
	now   func() time.Time

	limiter *rateLimiter
	cache   *preflightCache
	cost    *costTracker
}

// NewService builds the production service with an OpenAI-compatible provider.
func NewService(st *store.Store, hub *notify.Hub, cfg Config) *Service {
	cfg = cfg.withDefaults()
	var provider Provider
	if cfg.Enabled {
		provider = NewOpenAIProvider(cfg)
	}
	return NewServiceWithProvider(st, hub, cfg, provider)
}

// NewServiceWithProvider is the seam tests use to inject a deterministic fake.
// Passing a nil provider leaves the service disabled.
func NewServiceWithProvider(st *store.Store, hub *notify.Hub, cfg Config, provider Provider) *Service {
	cfg = cfg.withDefaults()
	if !cfg.Enabled {
		provider = nil
	}
	return &Service{
		store:   st,
		hub:     hub,
		cfg:     cfg,
		gw:      newGateway(provider, cfg.MaxAttempts),
		now:     time.Now,
		limiter: newRateLimiter(),
		cache:   newPreflightCache(500, 10*time.Minute),
		cost:    newCostTracker(cfg),
	}
}

// Enabled reports whether the assistant is configured and wired.
func (s *Service) Enabled() bool { return s.cfg.Enabled && s.gw.provider != nil }

// Model returns the configured model name for diagnostics.
func (s *Service) Model() string { return s.cfg.Model }

// PromptVersion returns the active prompt identifier.
func (s *Service) PromptVersion() string { return PromptVersion }

// EnqueueTriage schedules an asynchronous analysis for a newly created ticket.
// It is intentionally fire-and-forget: a queue failure is logged, never allowed
// to fail the ticket creation that already succeeded.
func (s *Service) EnqueueTriage(ctx context.Context, ticket models.Ticket) {
	if !s.Enabled() {
		return
	}
	_, err := s.store.AI.EnqueueJob(ctx, store.NewAIJob{
		TicketID:      ticket.ID,
		Kind:          "triage",
		InputHash:     inputHash(ticket),
		PromptVersion: PromptVersion,
	})
	if err != nil {
		slog.Error("enqueue ai triage", "ticket_id", ticket.ID, "error", err)
	}
}

// Preflight classifies the text a user is composing. It is synchronous, cached
// and rate-limited, and it never returns an error for a provider failure: the
// caller gets an empty, degraded result so the form works exactly as it did
// before AI existed.
func (s *Service) Preflight(ctx context.Context, actor service.Actor, in TriageInput) (TriageResult, error) {
	if !s.Enabled() {
		return TriageResult{}, ErrDisabled
	}
	if s.cost.overBudget() {
		slog.Warn("ai preflight skipped: monthly budget reached")
		return s.degradedResult(), nil
	}
	if !s.limiter.allow(actor.ID) {
		return TriageResult{}, ErrRateLimited
	}

	requester := !actor.Role.IsStaff()
	in.Title = Redact(in.Title, requester)
	in.Description = Redact(in.Description, requester)

	key := cacheKey(in, actor.ID, requester)
	if cached, ok := s.cache.get(key); ok {
		cached.Cached = true
		return cached, nil
	}

	var scope *int64
	if requester {
		scope = &actor.ID
	}
	candidates, err := s.store.AI.Candidates(ctx,
		buildMatchQuery(in.Title, in.Description), scope, s.cfg.MaxCandidates)
	if err != nil {
		// Candidate generation is best-effort; without it the classifier can
		// still suggest a category and priority.
		slog.Warn("ai duplicate candidates", "error", err)
		candidates = nil
	}

	analysisResult, err := s.analyze(ctx, in, candidates, requester, s.cfg.PreflightTimeout)
	if err != nil {
		slog.Warn("ai preflight failed", "error", err)
		return s.degradedResult(), nil
	}
	suggestions, duplicates := resultFromOutput(analysisResult.Output, candidates)

	built := TriageResult{
		Suggestions:   suggestions,
		Duplicates:    duplicates,
		Model:         analysisResult.Model,
		PromptVersion: analysisResult.PromptVersion,
	}
	s.cache.put(key, built)
	return built, nil
}

// Run drives the background worker until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		slog.Info("ai assistant disabled")
		return
	}
	slog.Info("ai triage worker started", "model", s.cfg.Model, "prompt", PromptVersion)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("ai triage worker stopped")
			return
		case <-ticker.C:
			// Drain a bounded batch so one busy second cannot starve the ticker.
			for i := 0; i < 10; i++ {
				processed, err := s.ProcessOnce(ctx)
				if err != nil {
					slog.Error("ai triage worker", "error", err)
					break
				}
				if !processed {
					break
				}
			}
		}
	}
}

// ProcessOnce claims and runs a single job. It returns whether a job was found,
// which makes the worker loop and tests share the same code path.
func (s *Service) ProcessOnce(ctx context.Context) (bool, error) {
	if !s.Enabled() {
		return false, nil
	}

	job, found, err := s.store.AI.ClaimJob(ctx, 2*time.Minute)
	if err != nil || !found {
		return false, err
	}
	if job.TicketID == nil {
		_ = s.store.AI.FailJob(ctx, job.ID, "job has no ticket", 1)
		return true, nil
	}

	ticket, err := s.store.Tickets.ByID(ctx, *job.TicketID)
	if err != nil {
		_ = s.store.AI.FailJob(ctx, job.ID, err.Error(), s.cfg.MaxAttempts)
		return true, nil
	}

	candidates, err := s.store.AI.Candidates(ctx,
		buildMatchQuery(ticket.Title, ticket.Description), nil, s.cfg.MaxCandidates)
	if err != nil {
		slog.Warn("ai job candidates", "ticket_id", ticket.ID, "error", err)
		candidates = nil
	}

	in := TriageInput{
		Title: ticket.Title, Description: ticket.Description,
		Category: ticket.Category, Priority: ticket.Priority,
	}
	result, err := s.analyze(ctx, in, candidates, false, s.cfg.Timeout)
	if err != nil {
		slog.Warn("ai job analysis failed", "ticket_id", ticket.ID, "error", err)
		_ = s.store.AI.FailJob(ctx, job.ID, err.Error(), s.cfg.MaxAttempts)
		return true, nil
	}

	suggestions := suggestFromOutput(result.Output, candidates, result.Model, result.PromptVersion)
	// The store fill needs the ticket id on every row.
	for i := range suggestions {
		suggestions[i].TicketID = ticket.ID
	}
	if err := s.store.AI.AddSuggestions(ctx, job.ID, suggestions); err != nil {
		_ = s.store.AI.FailJob(ctx, job.ID, err.Error(), s.cfg.MaxAttempts)
		return true, nil
	}

	if err := s.store.AI.FinishJob(ctx, job.ID, store.AIJobMetrics{
		Model: result.Model, LatencyMS: result.LatencyMS,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		CostUSD: result.CostUSD,
	}); err != nil {
		slog.Error("finish ai job", "job_id", job.ID, "error", err)
	}

	if s.hub != nil {
		s.hub.Publish(notify.Target{AllStaff: true}, notify.Event{
			Type:     "ai_suggestion_ready",
			Title:    fmt.Sprintf("Triage suggestions ready for %s", ticket.TicketNumber),
			Body:     ticket.Title,
			Level:    "info",
			TicketID: &ticket.ID,
			Payload: map[string]any{
				"count":          len(suggestions),
				"prompt_version": PromptVersion,
			},
		})
	}
	return true, nil
}

// analyze performs one provider call, parses the output and meters its cost.
func (s *Service) analyze(ctx context.Context, in TriageInput, candidates []store.AICandidate,
	requester bool, timeout time.Duration) (analysis, error) {

	if timeout <= 0 {
		timeout = s.cfg.PreflightTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	system, user := buildPrompt(in, candidates, requester)
	started := s.now()
	resp, err := s.gw.Chat(ctx, ChatRequest{
		System: system, User: user,
		MaxTokens: s.cfg.MaxOutputTokens, Temperature: 0,
	})
	if err != nil {
		return analysis{}, err
	}

	output, err := parseOutput(resp.Content, candidates)
	if err != nil {
		return analysis{}, err
	}

	cost := s.cost.charge(resp.PromptTokens, resp.CompletionTokens)
	return analysis{
		Output:           output,
		Model:            firstNonEmpty(resp.Model, s.cfg.Model),
		PromptVersion:    PromptVersion,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		LatencyMS:        s.now().Sub(started).Milliseconds(),
		CostUSD:          cost,
	}, nil
}

func (s *Service) degradedResult() TriageResult {
	return TriageResult{
		Suggestions:   map[string]*Suggestion{},
		Duplicates:    []Duplicate{},
		Model:         s.cfg.Model,
		PromptVersion: PromptVersion,
		Degraded:      true,
	}
}

// ListSuggestions returns the newest suggestion per kind for a ticket.
func (s *Service) ListSuggestions(ctx context.Context, ticketID int64) ([]store.AISuggestion, error) {
	return s.store.AI.ListSuggestions(ctx, ticketID)
}

// Suggestion loads one suggestion scoped to its ticket.
func (s *Service) Suggestion(ctx context.Context, ticketID, id int64) (store.AISuggestion, error) {
	return s.store.AI.SuggestionByID(ctx, ticketID, id)
}

// MarkSuggestion records a human decision on a suggestion.
func (s *Service) MarkSuggestion(ctx context.Context, id, userID int64, accepted bool) error {
	return s.store.AI.MarkSuggestion(ctx, id, userID, accepted)
}

// Stats reports queue health and acceptance counters.
func (s *Service) Stats(ctx context.Context) (store.AIStats, error) {
	return s.store.AI.Stats(ctx)
}

// SpentUSD exposes the process-local spend meter for the health endpoint.
func (s *Service) SpentUSD() float64 { return s.cost.spent() }

// --- input hashing ---

func inputHash(t models.Ticket) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s",
		t.Title, t.Description, t.Category, t.Priority, PromptVersion)))
	return hex.EncodeToString(sum[:])
}

// cacheKey includes the caller, not just their role. Duplicate candidates are
// scoped per requester, so two people typing the same words must not share a
// cached result: the second would see the first person's tickets.
func cacheKey(in TriageInput, actorID int64, requester bool) string {
	scope := fmt.Sprintf("staff:%d", actorID)
	if requester {
		scope = fmt.Sprintf("requester:%d", actorID)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s",
		in.Title, in.Description, in.Category, in.Priority, scope)))
	return hex.EncodeToString(sum[:])
}

// --- rate limiting ---

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: map[int64]*bucket{}}
}

// allow implements a token bucket: a burst of five preflights, refilling one
// every four seconds. That is generous for a person typing one ticket and too
// slow to be a useful cost amplifier.
func (l *rateLimiter) allow(userID int64) bool {
	if userID <= 0 {
		return false
	}
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[userID]
	if !ok {
		b = &bucket{tokens: 5, last: now}
		l.buckets[userID] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * 0.25
		if b.tokens > 5 {
			b.tokens = 5
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// --- preflight cache ---

type preflightCache struct {
	mu    sync.Mutex
	items map[string]cacheEntry
	max   int
	ttl   time.Duration
	now   func() time.Time
}

type cacheEntry struct {
	result  TriageResult
	expires time.Time
}

func newPreflightCache(max int, ttl time.Duration) *preflightCache {
	return &preflightCache{
		items: map[string]cacheEntry{}, max: max, ttl: ttl, now: time.Now,
	}
}

func (c *preflightCache) get(key string) (TriageResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok || c.now().After(entry.expires) {
		delete(c.items, key)
		return TriageResult{}, false
	}
	return entry.result, true
}

func (c *preflightCache) put(key string, result TriageResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A simple reset is enough: the cache exists to absorb typing bursts, not
	// to be a durable store.
	if len(c.items) >= c.max {
		c.items = map[string]cacheEntry{}
	}
	c.items[key] = cacheEntry{result: result, expires: c.now().Add(c.ttl)}
}

// --- spend metering ---

type costTracker struct {
	mu     sync.Mutex
	month  string
	spentV float64
	budget float64
	in     float64
	out    float64
}

func newCostTracker(cfg Config) *costTracker {
	return &costTracker{
		month:  time.Now().UTC().Format("2006-01"),
		budget: cfg.MonthlyBudgetUSD,
		in:     cfg.InputCostPerMTok,
		out:    cfg.OutputCostPerMTok,
	}
}

func (c *costTracker) charge(promptTokens, completionTokens int) float64 {
	cost := float64(promptTokens)/1e6*c.in + float64(completionTokens)/1e6*c.out
	c.mu.Lock()
	defer c.mu.Unlock()
	month := time.Now().UTC().Format("2006-01")
	if month != c.month {
		c.month, c.spentV = month, 0
	}
	c.spentV += cost
	return cost
}

func (c *costTracker) overBudget() bool {
	if c.budget <= 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spentV >= c.budget
}

func (c *costTracker) spent() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spentV
}
