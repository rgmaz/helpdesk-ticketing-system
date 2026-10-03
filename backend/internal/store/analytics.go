package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/models"
)

// AnalyticsRepo holds the aggregation queries behind the reporting endpoints.
//
// SQLite has no native interval type, so elapsed-time maths uses
// (julianday(b) - julianday(a)) * 24 for hours. That is exact enough at minute
// resolution and avoids the integer-second truncation of strftime('%s').
const hoursBetween = `((julianday(%s) - julianday(%s)) * 24.0)`

// MTTRResult summarises resolution performance over a window.
type MTTRResult struct {
	WindowDays    int      `json:"window_days"`
	ResolvedCount int      `json:"resolved_count"`
	AvgHours      *float64 `json:"avg_resolution_hours"`
	MedianHours   *float64 `json:"median_resolution_hours"`
	P50Hours      *float64 `json:"p50_resolution_hours"`
	P90Hours      *float64 `json:"p90_resolution_hours"`
	MinHours      *float64 `json:"min_resolution_hours"`
	MaxHours      *float64 `json:"max_resolution_hours"`
	AvgFirstResp  *float64 `json:"avg_first_response_hours"`
	MetSLA        int      `json:"sla_met_count"`
	BreachedSLA   int      `json:"sla_breached_count"`
	CompliancePct *float64 `json:"sla_compliance_pct"`
}

// ThroughputBucket is one day (or week) of resolution counts.
type ThroughputBucket struct {
	Bucket   string   `json:"bucket"`
	Created  int      `json:"created"`
	Resolved int      `json:"resolved"`
	AvgHours *float64 `json:"avg_resolution_hours"`
}

// CategoryBreakdown aggregates volume and resolution time by category.
type CategoryBreakdown struct {
	Category string   `json:"category"`
	Total    int      `json:"total"`
	Open     int      `json:"open"`
	Resolved int      `json:"resolved"`
	Breached int      `json:"breached"`
	AvgHours *float64 `json:"avg_resolution_hours"`
}

// PriorityBreakdown aggregates volume by priority band.
type PriorityBreakdown struct {
	Priority string   `json:"priority"`
	Total    int      `json:"total"`
	Open     int      `json:"open"`
	Resolved int      `json:"resolved"`
	Breached int      `json:"breached"`
	AvgHours *float64 `json:"avg_resolution_hours"`
}

// DashboardStats is the KPI block shown at the top of the admin dashboard.
type DashboardStats struct {
	TotalTickets      int      `json:"total_tickets"`
	OpenTickets       int      `json:"open_tickets"`
	ResolvedToday     int      `json:"resolved_today"`
	CreatedToday      int      `json:"created_today"`
	Unassigned        int      `json:"unassigned_tickets"`
	Breached          int      `json:"breached_tickets"`
	AtRisk            int      `json:"at_risk_tickets"`
	OverdueByHours    *float64 `json:"avg_overdue_hours"`
	TotalUsers        int      `json:"total_users"`
	ActiveTechnicians int      `json:"active_technicians"`
	AvgResponseHours  *float64 `json:"avg_first_response_hours"`
	CompliancePct     *float64 `json:"sla_compliance_pct"`
}

// MTTR computes mean time to resolution over the trailing windowDays.
// Only tickets with both created_at and resolved_at populated contribute, and
// the window is applied to resolved_at so recently resolved work is included.
func (r *AnalyticsRepo) MTTR(ctx context.Context, windowDays int) (MTTRResult, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays).Format(models.TimeLayout)

	out := MTTRResult{WindowDays: windowDays}

	hours := fmt.Sprintf(hoursBetween, "resolved_at", "created_at")
	err := r.db.QueryRowContext(ctx, `
		SELECT
		    COUNT(*),
		    AVG(`+hours+`),
		    MIN(`+hours+`),
		    MAX(`+hours+`),
		    AVG(CASE WHEN first_response_at IS NOT NULL
		             THEN `+fmt.Sprintf(hoursBetween, "first_response_at", "created_at")+` END),
		    COALESCE(SUM(CASE WHEN resolution_breached = 1 THEN 1 ELSE 0 END), 0),
		    COALESCE(SUM(CASE WHEN resolution_breached = 0 THEN 1 ELSE 0 END), 0)
		FROM tickets
		WHERE resolved_at IS NOT NULL AND resolved_at >= ?`, cutoff).
		Scan(&out.ResolvedCount, &out.AvgHours, &out.MinHours, &out.MaxHours,
			&out.AvgFirstResp, &out.BreachedSLA, &out.MetSLA)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}

	if out.ResolvedCount > 0 {
		total := out.MetSLA + out.BreachedSLA
		if total > 0 {
			pct := float64(out.MetSLA) / float64(total) * 100
			out.CompliancePct = &pct
		}
		p50, err := r.percentile(ctx, hours, cutoff, 0.50)
		if err != nil {
			return out, err
		}
		p90, err := r.percentile(ctx, hours, cutoff, 0.90)
		if err != nil {
			return out, err
		}
		out.P50Hours, out.P90Hours = p50, p90
		// With an even sample the median is the midpoint of the two middle
		// values, which is what p50 already returns; alias it for readability.
		out.MedianHours = p50
	}
	return out, nil
}

// percentile computes the p-th percentile of resolution hours using the
// row-offset method: LIMIT 1 OFFSET n, which avoids needing window functions.
func (r *AnalyticsRepo) percentile(ctx context.Context, hoursExpr, cutoff string, p float64) (*float64, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tickets
		WHERE resolved_at IS NOT NULL AND resolved_at >= ?`, cutoff).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	idx := int(float64(count-1) * p)

	var v sql.NullFloat64
	err := r.db.QueryRowContext(ctx, `
		SELECT `+hoursExpr+` FROM tickets
		WHERE resolved_at IS NOT NULL AND resolved_at >= ?
		ORDER BY `+hoursExpr+` ASC
		LIMIT 1 OFFSET ?`, cutoff, idx).Scan(&v)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if !v.Valid {
		return nil, nil
	}
	f := v.Float64
	return &f, nil
}

// Throughput returns daily created/resolved counts over the window, with no gaps.
func (r *AnalyticsRepo) Throughput(ctx context.Context, windowDays int) ([]ThroughputBucket, error) {
	if windowDays <= 0 || windowDays > 365 {
		windowDays = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays).Format(models.TimeLayout)
	hours := fmt.Sprintf(hoursBetween, "resolved_at", "created_at")

	// Recursive CTE generates every calendar day in the window so the chart has
	// a continuous x-axis even on days with zero activity.
	rows, err := r.db.QueryContext(ctx, `
		WITH RECURSIVE days(day) AS (
		    SELECT date(?)
		    UNION ALL
		    SELECT date(day, '+1 day') FROM days WHERE date(day, '+1 day') <= date(?)
		)
		SELECT
		    days.day,
		    (SELECT COUNT(*) FROM tickets t WHERE date(t.created_at) = days.day) AS created,
		    (SELECT COUNT(*) FROM tickets t WHERE t.resolved_at IS NOT NULL
		                          AND date(t.resolved_at) = days.day) AS resolved,
		    (SELECT AVG(`+hours+`) FROM tickets t
		       WHERE t.resolved_at IS NOT NULL AND date(t.resolved_at) = days.day) AS avg_hours
		FROM days
		ORDER BY days.day`, cutoff, time.Now().UTC().Format(models.TimeLayout))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ThroughputBucket{}
	for rows.Next() {
		var b ThroughputBucket
		var avg sql.NullFloat64
		if err := rows.Scan(&b.Bucket, &b.Created, &b.Resolved, &avg); err != nil {
			return nil, err
		}
		if avg.Valid {
			f := avg.Float64
			b.AvgHours = &f
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Workload returns per-technician active load, overdue count, and recent
// resolution performance. This is the routing report.
func (r *AnalyticsRepo) Workload(ctx context.Context) ([]models.TechnicianWorkload, error) {
	hours := fmt.Sprintf(hoursBetween, "resolved_at", "created_at")
	cutoff := time.Now().UTC().AddDate(0, 0, -30).Format(models.TimeLayout)

	rows, err := r.db.QueryContext(ctx, `
		SELECT
		    u.id, u.username, u.full_name,
		    COALESCE((SELECT COUNT(*) FROM tickets t
		              WHERE t.assignee_id = u.id AND t.status NOT IN ('closed')), 0) AS active_tickets,
		    COALESCE((SELECT COUNT(*) FROM tickets t
		              WHERE t.assignee_id = u.id AND t.status NOT IN ('closed')
		                AND t.resolution_due_at IS NOT NULL
		                AND t.resolution_due_at < strftime('%Y-%m-%dT%H:%M:%SZ','now')), 0) AS overdue,
		    COALESCE((SELECT COUNT(*) FROM tickets t
		              WHERE t.assignee_id = u.id AND t.resolved_at IS NOT NULL
		                AND t.resolved_at >= ?), 0) AS resolved_30d,
		    (SELECT AVG(`+hours+`) FROM tickets t
		       WHERE t.assignee_id = u.id AND t.resolved_at IS NOT NULL
		         AND t.resolved_at >= ?) AS avg_hours
		FROM users u
		JOIN roles r ON r.id = u.role_id
		WHERE r.name IN ('technician','admin') AND u.active = 1
		ORDER BY active_tickets DESC, u.username`, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.TechnicianWorkload{}
	for rows.Next() {
		var w models.TechnicianWorkload
		var avg sql.NullFloat64
		if err := rows.Scan(&w.UserID, &w.Username, &w.FullName,
			&w.ActiveTickets, &w.OverdueTickets, &w.Resolved30d, &avg); err != nil {
			return nil, err
		}
		if avg.Valid {
			f := avg.Float64
			w.AvgResolutionH = &f
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ByCategory aggregates volume and resolution time per category.
func (r *AnalyticsRepo) ByCategory(ctx context.Context, windowDays int) ([]CategoryBreakdown, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays).Format(models.TimeLayout)
	hours := fmt.Sprintf(hoursBetween, "resolved_at", "created_at")

	rows, err := r.db.QueryContext(ctx, `
		SELECT category,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status != 'closed' THEN 1 ELSE 0 END) AS open_n,
		       SUM(CASE WHEN resolved_at IS NOT NULL THEN 1 ELSE 0 END) AS resolved_n,
		       SUM(CASE WHEN resolution_breached = 1 THEN 1 ELSE 0 END) AS breached_n,
		       AVG(CASE WHEN resolved_at IS NOT NULL THEN `+hours+` END) AS avg_hours
		FROM tickets
		WHERE created_at >= ?
		GROUP BY category
		ORDER BY total DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CategoryBreakdown{}
	for rows.Next() {
		var c CategoryBreakdown
		var avg sql.NullFloat64
		if err := rows.Scan(&c.Category, &c.Total, &c.Open, &c.Resolved, &c.Breached, &avg); err != nil {
			return nil, err
		}
		if avg.Valid {
			f := avg.Float64
			c.AvgHours = &f
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ByPriority aggregates volume per priority band.
func (r *AnalyticsRepo) ByPriority(ctx context.Context, windowDays int) ([]PriorityBreakdown, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays).Format(models.TimeLayout)
	hours := fmt.Sprintf(hoursBetween, "resolved_at", "created_at")

	rows, err := r.db.QueryContext(ctx, `
		SELECT priority,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status != 'closed' THEN 1 ELSE 0 END) AS open_n,
		       SUM(CASE WHEN resolved_at IS NOT NULL THEN 1 ELSE 0 END) AS resolved_n,
		       SUM(CASE WHEN resolution_breached = 1 THEN 1 ELSE 0 END) AS breached_n,
		       AVG(CASE WHEN resolved_at IS NOT NULL THEN `+hours+` END) AS avg_hours
		FROM tickets
		WHERE created_at >= ?
		GROUP BY priority
		ORDER BY CASE priority WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PriorityBreakdown{}
	for rows.Next() {
		var p PriorityBreakdown
		var avg sql.NullFloat64
		if err := rows.Scan(&p.Priority, &p.Total, &p.Open, &p.Resolved, &p.Breached, &avg); err != nil {
			return nil, err
		}
		if avg.Valid {
			f := avg.Float64
			p.AvgHours = &f
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StatusDistribution returns counts per status, used for the donut chart.
func (r *AnalyticsRepo) StatusDistribution(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tickets GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for _, s := range models.AllStatuses {
		out[string(s)] = 0
	}
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// Dashboard returns the KPI block. atRiskMinutes defines the "approaching
// deadline" threshold.
func (r *AnalyticsRepo) Dashboard(ctx context.Context, atRiskMinutes int) (DashboardStats, error) {
	var s DashboardStats
	atRiskCutoff := time.Now().UTC().Add(time.Duration(atRiskMinutes) * time.Minute).
		Format(models.TimeLayout)
	todayStart := time.Now().UTC().Truncate(24 * time.Hour).Format(models.TimeLayout)

	err := r.db.QueryRowContext(ctx, `
		SELECT
		    (SELECT COUNT(*) FROM tickets),
		    (SELECT COUNT(*) FROM tickets WHERE status != 'closed'),
		    (SELECT COUNT(*) FROM tickets WHERE date(resolved_at) = date('now')),
		    (SELECT COUNT(*) FROM tickets WHERE date(created_at) = date('now')),
		    (SELECT COUNT(*) FROM tickets WHERE assignee_id IS NULL AND status != 'closed'),
		    (SELECT COUNT(*) FROM tickets WHERE resolution_breached = 1 OR response_breached = 1),
		    (SELECT COUNT(*) FROM tickets WHERE status != 'closed'
		       AND resolution_due_at IS NOT NULL AND resolution_due_at <= ?),
		    (SELECT AVG(CASE WHEN status != 'closed' AND resolution_due_at IS NOT NULL
		                      AND resolution_due_at < strftime('%Y-%m-%dT%H:%M:%SZ','now')
		                     THEN (julianday('now') - julianday(resolution_due_at)) * 24.0
		                     END) FROM tickets),
		    (SELECT COUNT(*) FROM users WHERE active = 1),
		    (SELECT COUNT(*) FROM users u JOIN roles r ON r.id = u.role_id
		      WHERE u.active = 1 AND r.name IN ('technician','admin')),
		    (SELECT AVG(CASE WHEN first_response_at IS NOT NULL
		                     THEN `+fmt.Sprintf(hoursBetween, "first_response_at", "created_at")+` END)
		     FROM tickets WHERE created_at >= ?),
		    (SELECT ROUND(100.0 * SUM(CASE WHEN resolution_breached = 0 THEN 1 ELSE 0 END)
		                  / NULLIF(COUNT(*), 0), 1)
		     FROM tickets WHERE resolved_at IS NOT NULL AND resolved_at >= ?)`,
		atRiskCutoff, todayStart, todayStart).
		Scan(&s.TotalTickets, &s.OpenTickets, &s.ResolvedToday, &s.CreatedToday,
			&s.Unassigned, &s.Breached, &s.AtRisk, &s.OverdueByHours,
			&s.TotalUsers, &s.ActiveTechnicians, &s.AvgResponseHours, &s.CompliancePct)
	if err != nil {
		return s, err
	}
	return s, nil
}

// ResponseTimeHistogram buckets first-response and resolution times into fixed
// bands for the dashboard bar chart.
type HistogramBucket struct {
	Label      string   `json:"label"`
	Count      int      `json:"count"`
	UpperBound *float64 `json:"upper_bound_hours"`
}

// ResponseHistogram buckets first-response times: <1h, 1-4h, 4-8h, 8-24h, >24h.
func (r *AnalyticsRepo) ResponseHistogram(ctx context.Context, windowDays int) ([]HistogramBucket, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -windowDays).Format(models.TimeLayout)
	hours := fmt.Sprintf(hoursBetween, "first_response_at", "created_at")

	bands := []struct {
		label string
		expr  string
		upper *float64
	}{
		{"<1h", hours + ` < 1`, ptr(1.0)},
		{"1-4h", hours + ` >= 1 AND ` + hours + ` < 4`, ptr(4.0)},
		{"4-8h", hours + ` >= 4 AND ` + hours + ` < 8`, ptr(8.0)},
		{"8-24h", hours + ` >= 8 AND ` + hours + ` < 24`, ptr(24.0)},
		{">24h", hours + ` >= 24`, nil},
	}

	out := make([]HistogramBucket, 0, len(bands))
	for _, b := range bands {
		var n int
		if err := r.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tickets
			WHERE first_response_at IS NOT NULL AND created_at >= ? AND (`+b.expr+`)`,
			cutoff).Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, HistogramBucket{Label: b.label, Count: n, UpperBound: b.upper})
	}
	return out, nil
}

func ptr(f float64) *float64 { return &f }

// AnalyticsRepo provides aggregate reporting queries.
type AnalyticsRepo struct{ db *sql.DB }
