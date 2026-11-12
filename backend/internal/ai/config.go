// Package ai provides the advisory triage layer: it classifies a ticket and
// finds possible duplicates, but it never mutates tickets itself. Every result
// is stored as a suggestion that a person must accept.
package ai

import "time"

// PromptVersion identifies the instruction set used to produce a suggestion.
// It is stored on every row so prompt changes can be evaluated and rolled back
// without guessing which version produced a given result.
const PromptVersion = "triage.v1"

// Config is the runtime configuration for the AI service. It is built from the
// process environment in main so this package does not depend on config.
type Config struct {
	Enabled           bool
	BaseURL           string
	APIKey            string
	Model             string
	Timeout           time.Duration
	PreflightTimeout  time.Duration
	MaxOutputTokens   int
	MonthlyBudgetUSD  float64
	InputCostPerMTok  float64
	OutputCostPerMTok float64
	MaxAttempts       int
	MaxCandidates     int
}

// withDefaults fills the values that must be sane even when the caller only
// flipped Enabled, which is what tests and embedders tend to do.
func (c Config) withDefaults() Config {
	if c.Timeout <= 0 {
		c.Timeout = 20 * time.Second
	}
	if c.PreflightTimeout <= 0 {
		c.PreflightTimeout = 3 * time.Second
	}
	if c.MaxOutputTokens <= 0 {
		c.MaxOutputTokens = 400
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = 20
	}
	if c.Model == "" {
		c.Model = "gpt-4o-mini"
	}
	return c
}
