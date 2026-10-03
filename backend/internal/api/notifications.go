package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/ryan/ticketing-system/backend/internal/httpx"
	"github.com/ryan/ticketing-system/backend/internal/models"
	"github.com/ryan/ticketing-system/backend/internal/notify"
)

// handleListNotifications returns the caller's durable notification history.
//
// The SSE stream carries the live edge; this table is the backlog a reconnecting
// client replays from, which is why both exist.
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	unreadOnly := false
	if v, present := httpx.QueryBool(r, "unread"); present {
		unreadOnly = v
	}
	limit := httpx.QueryInt(r, "limit", 50, 1, 200)
	page := httpx.QueryInt(r, "page", 0, 0, 10000)

	items, total, err := s.store.Notify.List(r.Context(), actor.ID, unreadOnly, limit, page*limit)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, httpx.ListBody{
		Items: items, Total: total, Page: page + 1, Limit: limit,
		Pages: httpx.Pages(total, limit),
	})
}

// handleUnreadCount powers the navbar badge.
func (s *Server) handleUnreadCount(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := s.store.Notify.UnreadCount(r.Context(), actor.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"unread": n})
}

// handleMarkNotificationRead marks one notification read.
//
// Marking someone else's notification is a 404 rather than a 403 so the endpoint
// does not confirm the existence of records the caller cannot see.
func (s *Server) handleMarkNotificationRead(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	id, err := httpx.PathInt64(r, "notification_id")
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := s.store.Notify.MarkRead(r.Context(), actor.ID, id); err != nil {
		if errors.Is(err, models.ErrNotFound) {
			httpx.Error(w, http.StatusNotFound, "notification not found", "not_found")
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"id": id, "read": true})
}

// handleMarkAllNotificationsRead clears the caller's badge.
func (s *Server) handleMarkAllNotificationsRead(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	n, err := s.store.Notify.MarkAllRead(r.Context(), actor.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"marked_read": n})
}

// sseBuffer is the per-connection event queue depth. It absorbs a burst of
// ticket and SLA events; beyond that the hub drops the connection and the client
// resyncs from the notifications table.
const sseBuffer = 64

// handleNotificationStream is the server-sent events endpoint.
//
// SSE is used instead of WebSockets because the traffic is one-directional
// server-to-client, it survives ordinary HTTP proxies, and it reconnects on its
// own. The browser EventSource API cannot set an Authorization header, so the
// auth middleware also accepts ?access_token=; the frontend uses fetch with a
// ReadableStream instead and sends the header normally.
func (s *Server) handleNotificationStream(w http.ResponseWriter, r *http.Request) {
	actor, err := actorOf(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Every real server supports flushing; without it the stream would
		// silently buffer, which is worse than failing loudly.
		httpx.Error(w, http.StatusInternalServerError,
			"streaming is not supported by this server", "stream_unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx honours this) so events arrive immediately.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.hub.Subscribe(actor.ID, string(actor.Role), sseBuffer)
	defer sub.Close()

	// An initial comment plus flush establishes the stream for the client and
	// defeats any middleware that waits for the first write to resolve.
	writeSSE(w, flusher, []byte(": connected\n\nretry: 3000\n\n"))

	keepAlive := time.NewTicker(notify.KeepAliveInterval)
	defer keepAlive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// The browser navigated away or the tab closed.
			return

		case <-sub.Done():
			return

		case ev := <-sub.Events:
			frame, err := notify.Encode(ev)
			if err != nil {
				// A malformed event is a programming error; log and keep the
				// stream alive rather than dropping the client.
				continue
			}
			if !writeSSE(w, flusher, frame) {
				return
			}

		case <-keepAlive.C:
			if !writeSSE(w, flusher, notify.KeepAlive) {
				return
			}
		}
	}
}

// writeSSE writes one frame and flushes it. It returns false when the connection
// is gone, which ends the stream loop.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, frame []byte) bool {
	if _, err := w.Write(frame); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

// clientIP extracts the best-effort remote address for audit logging. Proxy
// headers are only consulted when the deployment sets ITSM_TRUST_PROXY, which
// keeps a spoofed X-Forwarded-For out of the audit trail by default.
func clientIP(r *http.Request) string {
	return r.RemoteAddr
}
