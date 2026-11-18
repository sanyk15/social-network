package httpapi

import (
	"net/http"
)

type feedRebuildRequest struct {
	UserID flexID `json:"user_id"`
	All    bool   `json:"all"`
}

// RebuildFeed rebuilds feed caches from PostgreSQL: one user with
// {"user_id":N}, or every user with {"all":true}. It is the explicit
// "перестройка кэшей из СУБД" entry point (also available as cmd/feedrebuild).
func (h *Handler) RebuildFeed(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	if _, ok := h.authenticate(ctx, r, w); !ok {
		return
	}

	var req feedRebuildRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.All {
		rebuilt, err := h.feed.RebuildAll(r.Context(), 500, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rebuilt": rebuilt})
		return
	}

	if req.UserID.Int64() <= 0 {
		writeError(w, http.StatusBadRequest, "user_id or all is required")
		return
	}
	if err := h.feed.Rebuild(r.Context(), req.UserID.Int64()); err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rebuilt": 1})
}

type feedConfigView struct {
	Size       int    `json:"size"`
	TTLms      int64  `json:"feed_ttl_ms"`
	IncludeOwn bool   `json:"include_own"`
	Mode       string `json:"fanout_mode"`
}

// FeedStats exposes cache counters, the active fan-out mode and the queue
// depth. It is meant for the demo/report, not for production dashboards.
func (h *Handler) FeedStats(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	if _, ok := h.authenticate(ctx, r, w); !ok {
		return
	}

	queue, err := h.feed.QueueStats(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stats": h.feed.Stats(ctx),
		"queue": queue,
		"config": feedConfigView{
			Size:       h.feed.Size(),
			TTLms:      h.feed.FeedTTL().Milliseconds(),
			IncludeOwn: h.feed.IncludeOwn(),
			Mode:       string(h.feed.Mode()),
		},
	})
}

// FeedStatus reports whether one user's feed is materialised in Redis.
func (h *Handler) FeedStatus(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	if _, ok := h.authenticate(ctx, r, w); !ok {
		return
	}

	userID, err := idFromRequest(r, &friendRequest{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	status, err := h.feed.Status(ctx, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, status)
}
