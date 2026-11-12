package ai

import (
	"strings"
	"testing"

	"github.com/ryan/ticketing-system/backend/internal/store"
)

func TestBuildMatchQueryQuotesTermsAndDropsNoise(t *testing.T) {
	q := buildMatchQuery("VPN drops connection after the update", "")
	if q == "" {
		t.Fatal("expected a match expression")
	}
	if strings.ContainsAny(q, "()*") {
		t.Fatalf("match query must not contain FTS operators: %q", q)
	}
	if strings.Contains(q, `"the"`) {
		t.Fatalf("stopword leaked into match query: %q", q)
	}
	for _, want := range []string{`"vpn"`, `"drops"`, `"connection"`, `"update"`} {
		if !strings.Contains(q, want) {
			t.Fatalf("match query %q is missing %s", q, want)
		}
	}
	if !strings.Contains(q, " OR ") {
		t.Fatalf("terms should be OR-joined: %q", q)
	}
}

func TestBuildMatchQueryFallsBackToDescription(t *testing.T) {
	q := buildMatchQuery("", "printer jam on the third floor")
	if !strings.Contains(q, `"printer"`) || !strings.Contains(q, `"floor"`) {
		t.Fatalf("description terms missing from %q", q)
	}
}

func TestParseOutputRejectsUnknownVocabulary(t *testing.T) {
	_, err := parseOutput(`{"category":{"value":"billing","confidence":0.9},"priority":{"value":"urgent","confidence":0.9}}`, nil)
	if err == nil {
		t.Fatal("expected unknown category and priority to be rejected")
	}
}

func TestParseOutputDropsHallucinatedDuplicate(t *testing.T) {
	candidates := []store.AICandidate{
		{TicketID: 7, TicketNumber: "TKT-000007", Title: "VPN down"},
	}
	out, err := parseOutput(`{
		"category":{"value":"network","confidence":0.9,"rationale":"vpn"},
		"duplicates":[
			{"ticket_id":7,"score":0.8,"rationale":"same symptom"},
			{"ticket_id":999,"score":0.99,"rationale":"invented"}
		]
	}`, candidates)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if len(out.Duplicates) != 1 || out.Duplicates[0].TicketID != 7 {
		t.Fatalf("expected only the known candidate, got %+v", out.Duplicates)
	}
	if out.Category == nil || out.Category.Value != "network" {
		t.Fatalf("category was not preserved: %+v", out.Category)
	}
}

func TestParseOutputClampsScoresAndStripsFences(t *testing.T) {
	candidates := []store.AICandidate{{TicketID: 3, TicketNumber: "TKT-000003", Title: "Email down"}}
	out, err := parseOutput("```json\n"+
		`{"priority":{"value":"high","confidence":4,"rationale":" outage "},"duplicates":[{"ticket_id":3,"score":-2}]}`+
		"\n```", candidates)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if out.Priority == nil || out.Priority.Confidence != 1 {
		t.Fatalf("confidence should clamp to 1: %+v", out.Priority)
	}
	if len(out.Duplicates) != 1 || out.Duplicates[0].Score != 0 {
		t.Fatalf("score should clamp to 0: %+v", out.Duplicates)
	}
}

func TestParseOutputRejectsEmptySuggestions(t *testing.T) {
	if _, err := parseOutput(`{"duplicates":[]}`, nil); err == nil {
		t.Fatal("expected an error when nothing usable was returned")
	}
}

func TestCacheKeyIsPerCaller(t *testing.T) {
	in := TriageInput{Title: "VPN keeps dropping", Description: "every few minutes"}
	a := cacheKey(in, 1, true)
	b := cacheKey(in, 2, true)
	if a == b {
		t.Fatal("requester preflight results must not be shared between callers")
	}
	if a != cacheKey(in, 1, true) {
		t.Fatal("cache key is not deterministic")
	}
}

func TestRedactRemovesSecretsAndIdentity(t *testing.T) {
	in := "password=hunter2 contact bob@example.com from 10.0.0.4 token: abc123"
	out := Redact(in, true)
	for _, banned := range []string{"hunter2", "bob@example.com", "10.0.0.4", "abc123"} {
		if strings.Contains(out, banned) {
			t.Fatalf("redaction left %q in %q", banned, out)
		}
	}

	// Staff analysis keeps contact details but still removes credentials.
	staffView := Redact(in, false)
	if strings.Contains(staffView, "hunter2") || strings.Contains(staffView, "abc123") {
		t.Fatalf("credentials must always be redacted: %q", staffView)
	}
	if !strings.Contains(staffView, "bob@example.com") {
		t.Fatalf("staff view should retain non-secret context: %q", staffView)
	}
}
