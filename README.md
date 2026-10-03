# Service Desk

An IT service management ticketing system: a Go backend with JWT auth, role-based
access, full-text search, an SLA engine and live updates, and a React single-page
frontend with role-specific views.

```
backend/    Go 1.27 API, SQLite via modernc.org/sqlite (no CGO)
frontend/   Vite + React 19 + TypeScript
```

## Running it

Two terminals. The backend first:

```sh
cd backend
ITSM_JWT_SECRET=$(openssl rand -hex 32) go run . -demo
```

Then the frontend:

```sh
cd frontend
npm install
npm run dev
```

Open <http://localhost:5173>. The Vite dev server proxies `/api` to
`http://127.0.0.1:8080`, so the browser only ever talks to one origin and the app
cannot accidentally come to depend on CORS succeeding. Point it elsewhere with
`ITSM_BACKEND=http://127.0.0.1:9090 npm run dev`.

### Demo accounts

`-demo` seeds 40 tickets across four technicians and six requesters, and sets the
bootstrap administrator to the same documented password. Every demo account uses
**`DemoDesk1234`**:

| Username        | Role          | Sees                                        |
| --------------- | ------------- | ------------------------------------------- |
| `admin`         | Administrator | Dashboard, people, SLA policies             |
| `tech.avery`    | Technician    | Full queue, internal notes                  |
| `tech.blake`    | Technician    | Full queue, internal notes                  |
| `tech.casey`    | Technician    | Full queue, internal notes                  |
| `tech.devon`    | Technician    | Full queue, internal notes                  |
| `jordan.reed`   | Requester     | Only their own five tickets                 |
| `sam.ortiz`     | Requester     | Only their own tickets                      |
| `priya.nair`    | Requester     | Only their own tickets                      |
| `kai.lindgren`  | Requester     | Only their own tickets                      |
| `robin.ashby`   | Requester     | Only their own tickets                      |
| `morgan.deluca` | Requester     | Only their own tickets                      |

The sign-in screen only offers these credentials when the server was started with
`-demo`; it learns this from `GET /meta`, which publishes a boolean and never the
password itself. Without `-demo` the administrator gets a random password, logged
once at startup and never stored in configuration.

The flag is a development convenience and it is guarded: the seeder refuses to run
when tickets already exist, so pointing it at a real database creates nothing.

## Configuration

Every setting is an environment variable, so the same binary runs in development
and production. `ITSM_JWT_SECRET` is the only one with no default and it is
required — the process refuses to start without a secret of at least 16
characters.

| Variable                  | Default                       | Meaning                                              |
| ------------------------- | ----------------------------- | ---------------------------------------------------- |
| `ITSM_JWT_SECRET`         | *(required)*                  | HS256 signing key, ≥ 16 characters                   |
| `ITSM_HOST`               | `127.0.0.1`                   | Listen address                                       |
| `ITSM_PORT`               | `8080`                        | Listen port                                          |
| `ITSM_DB_PATH`            | `./data/itsm.db`              | SQLite file; created with its parent directory        |
| `ITSM_ALLOWED_ORIGINS`    | `http://localhost:5173,http://127.0.0.1:5173` | Comma-separated CORS allowlist        |
| `ITSM_ACCESS_TTL`         | `12h`                         | Token lifetime                                       |
| `ITSM_SLA_SCAN_INTERVAL`  | `1m`                          | SLA engine tick, ≥ 1s                                |
| `ITSM_SLA_WARN_BEFORE`    | `15m`                         | How early a deadline counts as "at risk"             |
| `ITSM_ADMIN_USERNAME`     | `admin`                       | Bootstrap administrator name                         |
| `ITSM_ADMIN_EMAIL`        | `admin@itsm.local`            | Bootstrap administrator email                        |
| `ITSM_ADMIN_PASSWORD`     | *(generated and logged)*      | Bootstrap password; unset generates a random one     |
| `ITSM_DEMO`               | `false`                       | Boolean; also settable with `-demo`                  |
| `ITSM_SHUTDOWN_TIMEOUT`   | `10s`                         | Grace period for in-flight requests on SIGINT        |

## How it fits together

### Storage

SQLite through `modernc.org/sqlite`, a pure-Go translation, so there is no CGO and
no build toolchain beyond Go itself. The connection opens in WAL mode with
`foreign_keys` on, a 5-second busy timeout, and a pool capped at eight
connections — WAL lets readers run concurrently with the SLA engine's writer,
which is the write pattern that matters here.

Timestamps are stored as `strftime('%Y-%m-%dT%H:%M:%SZ','now')` text so they sort
lexically and need no driver-specific format handling. Elapsed times are computed
in SQL with `julianday`, keeping the arithmetic in one place.

### Authentication and authorisation

Stateless HS256 JWTs. The role is a claim, so authorisation needs no database
round-trip, but the `active` flag is re-checked per request through an
`auth.ActiveChecker` seam: deactivating an account revokes its tokens
immediately instead of leaving a valid credential alive until it expires.

Three roles, and the boundaries are enforced in the handler layer as well as the
middleware:

- **Requester** — sees and comments on their own tickets. The scoping is applied
  in SQL, not filtered afterwards, so there is no code path where a requester's
  query returns somebody else's rows.
- **Technician** — full queue access, internal notes, manual reassignment.
- **Administrator** — everything, plus the people directory, SLA policies and
  reporting.

Requesters cannot set a priority above medium. That cap is applied **before** the
SLA policy is looked up, so a requester who asks for "critical" gets a
medium-priority ticket on medium deadlines rather than a medium label attached to
critical clocks — `TestEndUserPriorityIsCapped` pins this.

### Ticket lifecycle

`models.transitions` is the single transition table. `Status.NextStatuses()`
derives from it, and the API returns each ticket's `available_transitions`, so the
action buttons in the UI are built from the same table the server enforces
instead of a second copy of the rules that can drift.

| From          | To                                                   |
| ------------- | ---------------------------------------------------- |
| `open`        | `in_progress`, `pending`, `resolved`, `closed`        |
| `in_progress` | `pending`, `resolved`, `closed`                      |
| `pending`     | `in_progress`, `resolved`, `closed`                  |
| `resolved`    | `closed`, `reopened`                                 |
| `closed`      | `reopened`                                           |
| `reopened`    | `in_progress`, `pending`, `resolved`, `closed`        |

A ticket can go straight from `open` to `resolved` for the trivial cases — a
question answered, a password reset — without anyone marking it in progress
first. Anything else is a 422 with the reason.

Resolving or closing requires resolution notes. They become the ticket's
resolution text, which is what MTTR and the search index actually read — a closed
ticket with no notes tells the requester nothing and makes the reporting
meaningless.

### Automated routing

A new ticket goes to the active technician with the fewest open tickets. High and
critical tickets prefer technicians carrying fewer overdue ones, so a breach does
not compound on whoever is already behind. Ties break on the lowest user id to
keep assignment deterministic and testable. Administrators are never
auto-assigned — they are escalation destinations, and putting them in the pool
would quietly drain the front line.

### SLA engine

One goroutine on a `time.Ticker`, defaulting to a minute. Each pass finds
deadlines that have passed, flags them, and then escalates on a policy-defined
interval so a breach and its escalation do not arrive in the same instant. Level
one raises priority; level two pulls in an administrator and reassigns. Passes are
idempotent, so a ticket is not re-escalated on every tick.

Seeded policies, one per priority:

| Priority | Response | Resolution | Escalates after |
| -------- | -------- | ---------- | --------------- |
| Critical | 15m      | 4h         | 30m             |
| High     | 1h       | 8h         | 60m             |
| Medium   | 8h       | 48h        | 120m            |
| Low      | 24h      | 72h        | none            |

A ticket inherits its policy at creation and keeps it, so tightening a target
never rewrites what history says was promised.

### Search

SQLite FTS5 over title, description and resolution notes, with porter stemming, so
`charg` finds "charging". Queries are plain words only — FTS5's operators are
rejected with a 400 rather than passed through, because a stray `AND` is a syntax
error to a user and a foot-gun for anyone who thinks they can inject operators.
Results carry real `snippet()` excerpts, fetched in one batched query rather than
per result.

### Live updates

Server-sent events on `GET /api/v1/notifications/stream`. SSE rather than
WebSockets because the traffic is one-directional, and it survives ordinary HTTP
infrastructure without an upgrade dance. The hub drops a consumer that cannot keep
up rather than blocking the broadcaster.

The browser client reads the stream with `fetch` and a `ReadableStream` instead of
`EventSource`, because `EventSource` cannot set an `Authorization` header and
passing a JWT in the query string leaks it into proxy and access logs. The server
accepts `?access_token=` as a fallback for non-browser clients.

Events are also persisted per user, so a client that was offline replays from
`GET /api/v1/notifications` instead of losing them.

## API

Base path `/api/v1`. Health is at `/health`, outside the version prefix.
Errors are `{"error": "...", "code": "..."}`.

### Public

| Method | Path               | Purpose                                            |
| ------ | ------------------ | -------------------------------------------------- |
| GET    | `/health`          | Liveness, plus SLA engine and stream client counts |
| POST   | `/auth/login`      | Exchange credentials for a token                   |
| POST   | `/auth/register`   | Create a requester account                         |
| GET    | `/meta`            | Statuses, priorities, categories, policies, sorts  |

`POST /auth/register` returns the account and **no token**. An unauthenticated
self-service endpoint that could mint sessions would mean anyone who can create
an account can also sign in as one, so registration is followed by a normal login.

### Any authenticated user

| Method | Path                                          | Purpose                        |
| ------ | --------------------------------------------- | ------------------------------ |
| GET    | `/auth/me`                                    | Profile and ticket counts      |
| POST   | `/auth/password`                              | Change own password            |
| GET    | `/tickets`                                    | List, filtered and sorted      |
| GET    | `/tickets/{id}`                               | Ticket, comments, events, SLA  |
| GET    | `/tickets/search?q=`                          | FTS5 search with snippets      |
| GET    | `/tickets/{id}/comments`                      | Comment thread                 |
| POST   | `/tickets/{id}/comments`                      | Add a comment or internal note |
| GET    | `/tickets/{id}/events`                        | Timeline                       |
| GET    | `/notifications`                              | History                        |
| GET    | `/notifications/unread-count`                 | Badge count                    |
| POST   | `/notifications/{id}/read`                    | Mark one read                  |
| POST   | `/notifications/read-all`                     | Clear the badge                |
| GET    | `/notifications/stream`                       | SSE                            |
| GET    | `/categories`, `/sla-policies`, `/roles`      | Reference data                 |

### Writes

`POST /tickets` and `PATCH /tickets/{id}` are open to any authenticated user —
the handler decides what a requester may change, since the rules depend on the
ticket's current state. `DELETE /tickets/{id}` is admin only.

### Staff

`GET /users`, `GET /technicians`, `GET /system/sla-engine`.

### Admin only

`POST`/`PATCH`/`DELETE` on `/users`, `PATCH /sla-policies/{id}`, all of
`/analytics/*`, and `GET /audit`.

### Listing

`GET /tickets` takes `status`, `priority`, `category`, `assignee`, `requester`,
`sla`, `q`, `sort_by`, `sort_dir`, `page` and `limit`.

`assignee` and `requester` accept an id, or `me` / `none`. Sort keys are the
server's whitelist — `created`, `updated`, `priority`, `status`, `due`, `number`,
`comment_count`, `requester_name`, `assignee_name` — and an unknown key is a 400
listing the valid ones rather than a silent fallback. The list response carries
per-status `counts` for the filter chips; the search response does not.

Search is restricted to plain words. `: " ^ ( ) * - { } [ ] ,` and bare
`AND`/`OR`/`NOT`/`NEAR` are rejected with a 400.

## Frontend

Three role-shaped areas behind `RequireRole` guards:

- **Portal** (`/portal`) — a requester's own tickets.
- **Queue** (`/queue`) — the staff work list, with load indicators.
- **Dashboard** (`/dashboard`), **People** (`/team`) and **Policies**
  (`/policies`) — admin only, each with a matching guard.

The guards are a convenience, not the security boundary. The server enforces
every rule and row-level scoping happens in SQL, so a client that edits its way
past a guard sees an empty list rather than another user's tickets. What the guard
prevents is offering a page that could only ever 403.

`GET /tickets/{id}` returns `available_transitions` and `can_delete`, so the UI
renders exactly the actions that are legal rather than guessing and letting the
server refuse.

The administrator pages are lazily loaded. The dashboard pulls in Recharts, which
is larger than the rest of the app combined and only an administrator ever needs
it, so splitting it out keeps that weight out of the bundle every other user
downloads.

Modals are built on the native `<dialog>` element, which gives focus trapping,
Escape handling and top-layer stacking from the platform rather than from a
dependency. They are mounted only while open, which is what lets each form start
blank without a reset effect.

### Scripts

```sh
npm run dev       # dev server with the API proxy
npm run build     # type-check and build to dist/
npm run lint      # oxlint
```

## Tests

```sh
cd backend
go test ./...
go test ./... -race     # slower; bcrypt dominates
```

Coverage is layered: `models` tests the pure domain rules — priority ranking, the
transition table, SLA timers, timestamp scanning — while `api` tests drive real
HTTP through a harness with a real database, so RBAC, row-level scoping, the FTS5
path and the SLA engine are all exercised end to end rather than mocked.

`go vet` and `gofmt` are both clean.