package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"social-network/internal/store"
)

const maxPostTextLen = 4096

// flexID accepts both JSON numbers and strings for identifiers. The course
// specification models ids as strings while this project uses BIGSERIAL, so
// accepting both keeps the endpoint compatible with either client.
type flexID int64

func (f *flexID) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(strings.Trim(string(b), `"`))
	if raw == "" {
		return errors.New("empty id")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q", raw)
	}
	*f = flexID(n)
	return nil
}

func (f flexID) Int64() int64 { return int64(f) }

type postCreateRequest struct {
	Text string `json:"text"`
}

type postUpdateRequest struct {
	ID   flexID `json:"id"`
	Text string `json:"text"`
}

type postDeleteRequest struct {
	ID flexID `json:"id"`
}

type friendRequest struct {
	UserID flexID `json:"user_id"`
}

func (h *Handler) CreatePost(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	userID, ok := h.authenticate(ctx, r, w)
	if !ok {
		return
	}

	var req postCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	text, err := validatePostText(req.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.feed.CreatePost(ctx, userID, text)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("create post: user %d: %v", userID, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "post": p})
}

func (h *Handler) UpdatePost(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	userID, ok := h.authenticate(ctx, r, w)
	if !ok {
		return
	}

	var req postUpdateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID.Int64() <= 0 {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	text, err := validatePostText(req.Text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.feed.UpdatePost(ctx, req.ID.Int64(), userID, text)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "post not found")
			return
		}
		log.Printf("update post %d: %v", req.ID.Int64(), err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "post": p})
}

// DeletePost accepts both the body form POST /post/delete {"id":N} and the
// path form PUT /post/delete/{id} from the course specification.
func (h *Handler) DeletePost(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	userID, ok := h.authenticate(ctx, r, w)
	if !ok {
		return
	}

	id, err := idFromRequest(r, &postDeleteRequest{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.feed.DeletePost(ctx, id, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "post not found")
			return
		}
		log.Printf("delete post %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GetPost serves both GET /post/get?id=N and GET /post/get/{id}.
func (h *Handler) GetPost(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	if _, ok := h.authenticate(ctx, r, w); !ok {
		return
	}

	id, err := idFromRequest(r, &postDeleteRequest{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	p, err := h.feed.GetPost(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "post not found")
			return
		}
		log.Printf("get post %d: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, p)
}

// Feed returns the cached friends feed as a JSON array (по спецификации —
// массив постов, а не объект с полем feed).
func (h *Handler) Feed(w http.ResponseWriter, r *http.Request) {
	ctx := h.store.WithReadOnly(r.Context())
	userID, ok := h.authenticate(ctx, r, w)
	if !ok {
		return
	}

	q := r.URL.Query()
	offset, err := intParam(q.Get("offset"), 0)
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative number")
		return
	}
	limit, err := intParam(q.Get("limit"), 10)
	if err != nil || limit < 1 {
		writeError(w, http.StatusBadRequest, "limit must be a positive number")
		return
	}

	posts, err := h.feed.Feed(ctx, userID, offset, limit)
	if err != nil {
		log.Printf("feed user %d: %v", userID, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, posts)
}

// FriendAdd serves POST /friend/add {"user_id":N} and PUT /friend/set/{user_id}.
func (h *Handler) FriendAdd(w http.ResponseWriter, r *http.Request) {
	h.friendWrite(w, r, true)
}

// FriendDelete serves POST /friend/delete {"user_id":N} and
// PUT /friend/delete/{user_id}.
func (h *Handler) FriendDelete(w http.ResponseWriter, r *http.Request) {
	h.friendWrite(w, r, false)
}

func (h *Handler) friendWrite(w http.ResponseWriter, r *http.Request, add bool) {
	ctx := h.store.WithReadOnly(r.Context())
	userID, ok := h.authenticate(ctx, r, w)
	if !ok {
		return
	}

	friendID, err := idFromRequest(r, &friendRequest{})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if friendID == userID {
		writeError(w, http.StatusBadRequest, "cannot add yourself as a friend")
		return
	}

	if add {
		created, err := h.feed.AddFriend(ctx, userID, friendID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "user not found")
				return
			}
			log.Printf("add friend %d -> %d: %v", userID, friendID, err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created})
		return
	}

	if err := h.feed.RemoveFriend(ctx, userID, friendID); err != nil {
		log.Printf("remove friend %d -> %d: %v", userID, friendID, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validatePostText(text string) (string, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return "", errors.New("text is required")
	case utf8.RuneCountInString(text) > maxPostTextLen:
		return "", fmt.Errorf("text must be at most %d characters", maxPostTextLen)
	}
	return text, nil
}

// idFromRequest reads an id from the path segment (/{id} or /{user_id}) or, if
// there is no path segment, from the JSON body. The spec uses path ids for
// some methods and the homework description names body-style methods; both
// forms work.
func idFromRequest(r *http.Request, body any) (int64, error) {
	for _, name := range []string{"id", "user_id"} {
		if raw := r.PathValue(name); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id <= 0 {
				return 0, fmt.Errorf("invalid %s", name)
			}
			return id, nil
		}
	}
	if raw := r.URL.Query().Get("id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return 0, errors.New("invalid id")
		}
		return id, nil
	}
	if raw := r.URL.Query().Get("user_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return 0, errors.New("invalid user_id")
		}
		return id, nil
	}

	if err := decodeJSONBody(r, body); err != nil {
		return 0, err
	}
	var id int64
	switch v := body.(type) {
	case *postDeleteRequest:
		id = v.ID.Int64()
	case *friendRequest:
		id = v.UserID.Int64()
	}
	if id <= 0 {
		return 0, errors.New("id is required")
	}
	return id, nil
}

// decodeJSONBody is decodeJSON for requests that have already been limited by
// the caller. Unknown fields are rejected to catch typos.
func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func intParam(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// authenticate validates the bearer token and returns the current user id.
func (h *Handler) authenticate(ctx context.Context, r *http.Request, w http.ResponseWriter) (int64, bool) {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
		return 0, false
	}
	userID, err := h.store.UserIDByToken(ctx, token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return 0, false
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return 0, false
	}
	return userID, true
}
