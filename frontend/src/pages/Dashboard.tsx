import { useState } from 'react'
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Legend,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api } from '../api/client'
import type { DashboardPayload } from '../api/types'
import { Card, Empty, ErrorNote, Spinner, Stat } from '../components/ui'
import { useAsync } from '../hooks/useAsync'
import { useLiveRefresh } from '../hooks/useLiveRefresh'
import { hours, humanize } from '../lib/format'
import { useMeta } from '../state/MetaContext'

/**
 * The administrator dashboard.
 *
 * One endpoint returns every panel, because the first paint is eight requests
 * otherwise and the aggregates all share the same window. The window itself is
 * a control rather than a constant because "this month" and "this quarter" are
 * different questions and the numbers mean little without it.
 */
export function DashboardPage() {
  const [windowDays, setWindowDays] = useState(30)
  const { meta } = useMeta()

  const { data, error, loading, initial, reload } = useAsync<DashboardPayload>(
    async () => api.dashboard(windowDays),
    [windowDays],
  )

  // A breach or an escalation changes every number on this page, so the
  // dashboard follows the stream rather than waiting for a manual refresh.
  useLiveRefresh(
    [
      'ticket_created',
      'status_changed',
      'ticket_resolved',
      'ticket_closed',
      'sla_escalation',
      'sla_resolution_breach',
      'sla_response_breach',
    ],
    reload,
  )

  if (initial && loading) return <Spinner label="Loading dashboard…" />
  if (error) return <ErrorNote>{error}</ErrorNote>
  if (!data) return <Spinner />

  const { stats, mttr, throughput, workload, by_category, by_priority, status_distribution, response_histogram, sla_engine } =
    data

  const statusRows = Object.entries(status_distribution)
    .map(([status, count]) => ({ status, label: humanize(status), count }))
    .sort((a, b) => b.count - a.count)

  const priorityOrder = (meta?.priorities ?? []).map((p) => p.value)
  const priorityRows = priorityOrder
    .map((value) => {
      const row = by_priority.find((p) => p.priority === value)
      return { priority: value, label: humanize(value), total: row?.total ?? 0, breached: row?.breached ?? 0 }
    })
    .filter((row) => row.total > 0)

  // Compliance is the headline number a service desk is judged on, but only once
  // there is enough data for a percentage to mean anything.
  const compliance =
    mttr.sla_compliance_pct !== null && mttr.resolved_count > 0 ? mttr.sla_compliance_pct : null

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Dashboard</h1>
          <p>
            Live figures over the trailing {windowDays} days. Updated{' '}
            {new Date(data.generated_at).toLocaleTimeString()}.
          </p>
        </div>
        <div className="row">
          <label htmlFor="window" className="nowrap">
            Window
          </label>
          <select
            id="window"
            value={windowDays}
            onChange={(event) => setWindowDays(Number(event.target.value))}
            style={{ width: 'auto' }}
          >
            <option value={7}>7 days</option>
            <option value={30}>30 days</option>
            <option value={90}>90 days</option>
            <option value={365}>1 year</option>
          </select>
          <button type="button" className="btn" onClick={reload}>
            Refresh
          </button>
        </div>
      </div>

      <div className="stat-grid" style={{ marginBottom: 16 }}>
        <Stat label="Open tickets" value={stats.open_tickets} sub={`${stats.total_tickets} total`} />
        <Stat
          label="Unassigned"
          value={stats.unassigned_tickets}
          sub={stats.unassigned_tickets > 0 ? 'Nobody is working these' : 'All assigned'}
          tone={stats.unassigned_tickets > 0 ? 'warn' : 'neutral'}
        />
        <Stat
          label="Breached"
          value={stats.breached_tickets}
          sub={stats.at_risk_tickets > 0 ? `${stats.at_risk_tickets} at risk` : 'None at risk'}
          tone={stats.breached_tickets > 0 ? 'alert' : 'good'}
        />
        <Stat
          label="SLA compliance"
          value={compliance === null ? '—' : `${compliance.toFixed(0)}%`}
          sub={
            compliance === null
              ? 'No resolved tickets in this window'
              : `${mttr.sla_met_count} met, ${mttr.sla_breached_count} missed`
          }
          tone={
            compliance === null ? 'neutral' : compliance >= 95 ? 'good' : compliance >= 80 ? 'warn' : 'alert'
          }
        />
        <Stat
          label="Mean time to resolve"
          value={hours(mttr.avg_resolution_hours)}
          sub={
            mttr.median_resolution_hours !== null
              ? `Median ${hours(mttr.median_resolution_hours)} · p90 ${hours(mttr.p90_resolution_hours)}`
              : `${mttr.resolved_count} resolved`
          }
        />
        <Stat
          label="First response"
          value={hours(mttr.avg_first_response_hours)}
          sub={`${stats.active_technicians} technician${stats.active_technicians === 1 ? '' : 's'} active`}
        />
      </div>

      <div className="chart-grid" style={{ marginBottom: 16 }}>
        <Card title="Created versus resolved">
          <Chart height={260}>
            <AreaChart data={throughput} margin={{ top: 4, right: 8, bottom: 0, left: -18 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis dataKey="bucket" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} />
              <YAxis tick={{ fontSize: 11, fill: 'var(--text-muted)' }} allowDecimals={false} />
              <Tooltip content={<ChartTooltip />} />
              <Legend wrapperStyle={{ fontSize: 12 }} />
              <Area
                type="monotone"
                dataKey="created"
                name="Created"
                stroke="var(--accent)"
                fill="var(--accent)"
                fillOpacity={0.15}
              />
              <Area
                type="monotone"
                dataKey="resolved"
                name="Resolved"
                stroke="var(--ok)"
                fill="var(--ok)"
                fillOpacity={0.15}
              />
            </AreaChart>
          </Chart>
        </Card>

        <Card title="Resolution time by bucket">
          <Chart height={260}>
            <BarChart data={throughput} margin={{ top: 4, right: 8, bottom: 0, left: -6 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis dataKey="bucket" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} />
              <YAxis
                tick={{ fontSize: 11, fill: 'var(--text-muted)' }}
                tickFormatter={(v) => hours(v, 0)}
              />
              <Tooltip content={<ChartTooltip />} formatter={(v) => hours(Number(v))} />
              <Bar dataKey="avg_resolution_hours" name="Avg hours" fill="var(--accent)" radius={[4, 4, 0, 0]} />
            </BarChart>
          </Chart>
        </Card>
      </div>

      <div className="chart-grid" style={{ marginBottom: 16 }}>
        <Card title="Tickets by status">
          {statusRows.length === 0 ? (
            <p className="muted">No tickets yet.</p>
          ) : (
            <Chart height={220}>
              <PieChart>
                <Pie
                  data={statusRows}
                  dataKey="count"
                  nameKey="label"
                  innerRadius="55%"
                  outerRadius="80%"
                  paddingAngle={2}
                >
                  {statusRows.map((row) => (
                    <Cell key={row.status} fill={statusColor(row.status)} />
                  ))}
                </Pie>
                <Tooltip content={<ChartTooltip />} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
              </PieChart>
            </Chart>
          )}
        </Card>

        <Card title="Volume by priority">
          {priorityRows.length === 0 ? (
            <p className="muted">No tickets in this window.</p>
          ) : (
            <Chart height={220}>
              <BarChart data={priorityRows} margin={{ top: 4, right: 8, bottom: 0, left: -18 }}>
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
                <XAxis dataKey="label" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} />
                <YAxis tick={{ fontSize: 11, fill: 'var(--text-muted)' }} allowDecimals={false} />
                <Tooltip content={<ChartTooltip />} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar dataKey="total" name="Total" fill="var(--accent)" radius={[4, 4, 0, 0]} />
                <Bar dataKey="breached" name="Breached" fill="var(--danger)" radius={[4, 4, 0, 0]} />
              </BarChart>
            </Chart>
          )}
        </Card>
      </div>

      <div className="chart-grid" style={{ marginBottom: 16 }}>
        <Card title="First response times">
          <Chart height={220}>
            <BarChart data={response_histogram} margin={{ top: 4, right: 8, bottom: 0, left: -18 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" vertical={false} />
              <XAxis dataKey="label" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} />
              <YAxis tick={{ fontSize: 11, fill: 'var(--text-muted)' }} allowDecimals={false} />
              <Tooltip content={<ChartTooltip />} />
              <Bar dataKey="count" name="Tickets" fill="var(--warn)" radius={[4, 4, 0, 0]} />
            </BarChart>
          </Chart>
        </Card>

        <Card title="Resolution by category">
          {by_category.length === 0 ? (
            <p className="muted">No tickets in this window.</p>
          ) : (
            <Chart height={220}>
              <BarChart
                data={by_category.slice(0, 8)}
                layout="vertical"
                margin={{ top: 4, right: 16, bottom: 0, left: 8 }}
              >
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" horizontal={false} />
                <XAxis type="number" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} allowDecimals={false} />
                <YAxis
                  type="category"
                  dataKey="category"
                  tick={{ fontSize: 11, fill: 'var(--text-muted)' }}
                  width={86}
                  tickFormatter={humanize}
                />
                <Tooltip content={<ChartTooltip />} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar dataKey="total" name="Total" stackId="a" fill="var(--accent)" />
                <Bar dataKey="breached" name="Breached" stackId="a" fill="var(--danger)" />
              </BarChart>
            </Chart>
          )}
        </Card>
      </div>

      <div className="chart-grid">
        <Card title="Workload by technician" padded={false}>
          {workload.length === 0 ? (
            <Empty title="No technicians" hint="No active technician accounts exist yet." />
          ) : (
            <Chart height={Math.max(200, workload.length * 38)}>
              <BarChart
                data={workload.map((tech) => ({
                  name: tech.full_name || tech.username,
                  active: tech.active_tickets,
                  overdue: tech.overdue_tickets,
                }))}
                margin={{ top: 8, right: 16, bottom: 0, left: 8 }}
              >
                <CartesianGrid strokeDasharray="3 3" stroke="var(--border)" horizontal={false} />
                <XAxis type="number" tick={{ fontSize: 11, fill: 'var(--text-muted)' }} allowDecimals={false} />
                <YAxis
                  type="category"
                  dataKey="name"
                  tick={{ fontSize: 11, fill: 'var(--text-muted)' }}
                  width={110}
                />
                <Tooltip content={<ChartTooltip />} />
                <Legend wrapperStyle={{ fontSize: 12 }} />
                <Bar dataKey="active" name="Active" stackId="a" fill="var(--accent)" />
                <Bar dataKey="overdue" name="Overdue" stackId="a" fill="var(--danger)" radius={[0, 4, 4, 0]} />
              </BarChart>
            </Chart>
          )}
        </Card>

        <Card title="SLA engine">
          <div className="stat-grid">
            <Stat label="Breaches detected" value={sla_engine.breaches_detected} />
            <Stat label="Escalations" value={sla_engine.escalations} />
            <Stat
              label="Scan interval"
              value={`${Math.round(sla_engine.interval_seconds)}s`}
              sub={`${sla_engine.ticks} scans`}
            />
            <Stat
              label="Errors"
              value={sla_engine.errors}
              tone={sla_engine.errors > 0 ? 'alert' : 'good'}
            />
          </div>
          {sla_engine.last_error && (
            <p className="muted" style={{ fontSize: '0.8125rem', marginTop: 12 }}>
              Last error: {sla_engine.last_error}
            </p>
          )}
          <p className="faint" style={{ fontSize: '0.75rem', marginTop: 12 }}>
            The engine runs in a single goroutine on a timer. A ticket is flagged the moment its
            deadline passes, then escalated a policy-defined interval later.
          </p>
        </Card>
      </div>
    </>
  )
}

/** Recharts needs a sized parent; the chart fills whatever box it is given. */
function Chart({ height, children }: { height: number; children: React.ReactElement }) {
  return (
    <div style={{ width: '100%', height }}>
      <ResponsiveContainer width="100%" height="100%">
        {children}
      </ResponsiveContainer>
    </div>
  )
}

/**
 * A tooltip that formats the values this domain uses.
 *
 * Recharts' default prints raw numbers, so "41.833333333333336" turns up in a
 * management report. Nulls are labelled rather than shown as a gap, because an
 * average over no rows is a real answer.
 */
function ChartTooltip({
  active,
  payload,
  label,
}: {
  active?: boolean
  payload?: { name?: string; value?: number | string; color?: string }[]
  label?: string | number
}) {
  if (!active || !payload?.length) return null
  return (
    <div
      className="card"
      style={{ padding: '8px 11px', fontSize: '0.8125rem', boxShadow: 'var(--shadow-lg)' }}
    >
      {label !== undefined && <div style={{ fontWeight: 600, marginBottom: 4 }}>{humanize(String(label))}</div>}
      {payload.map((entry, i) => (
        <div key={i} className="row" style={{ gap: 6 }}>
          <span
            style={{ width: 8, height: 8, borderRadius: 2, background: entry.color, display: 'inline-block' }}
          />
          <span className="muted">{entry.name}</span>
          <strong>{typeof entry.value === 'number' ? entry.value : entry.value ?? '—'}</strong>
        </div>
      ))}
    </div>
  )
}

/** Status colours match the badges, so a pie slice reads like the table. */
function statusColor(status: string): string {
  switch (status) {
    case 'closed':
      return 'var(--text-faint)'
    case 'resolved':
      return 'var(--ok)'
    case 'pending':
      return 'var(--warn)'
    case 'reopened':
      return 'var(--danger)'
    default:
      return 'var(--accent)'
  }
}
