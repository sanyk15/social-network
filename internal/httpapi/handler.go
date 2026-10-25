package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"social-network/internal/store"
)

const maxBodyBytes = 1 << 20

type Handler struct {
	store *store.Store
	ttl   time.Duration
}

func New(s *store.Store, ttl time.Duration) *Handler {
	return &Handler{store: s, ttl: ttl}
}

type registerRequest struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	FirstName string   `json:"first_name"`
	LastName  string   `json:"last_name"`
	BirthDate string   `json:"birth_date"`
	Gender    string   `json:"gender"`
	Interests []string `json:"interests"`
	City      string   `json:"city"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRegister(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("register: hash password: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	interests := req.Interests
	if interests == nil {
		interests = []string{}
	}

	u := store.User{
		Username:  req.Username,
		FirstName: req.FirstName,
		LastName:  req.LastName,
		BirthDate: req.BirthDate,
		Gender:    req.Gender,
		Interests: interests,
		City:      req.City,
	}

	id, err := h.store.CreateUser(r.Context(), u, string(hash))
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "username already taken")
			return
		}
		log.Printf("register: create user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	cred, err := h.store.GetCredentialByUsername(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid credentials")
			return
		}
		log.Printf("login: get credential: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(req.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	token, err := generateToken()
	if err != nil {
		log.Printf("login: generate token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.store.CreateSession(r.Context(), token, cred.ID, h.ttl); err != nil {
		log.Printf("login: create session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	u, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("get user: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, u)
}

// Search returns profiles whose first and last names start with the given
// prefixes. Both query parameters are required.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r) {
		return
	}

	q := r.URL.Query()
	firstName := strings.TrimSpace(q.Get("first_name"))
	lastName := strings.TrimSpace(q.Get("last_name"))
	if firstName == "" || lastName == "" {
		writeError(w, http.StatusBadRequest, "first_name and last_name are required")
		return
	}

	users, err := h.store.SearchUsers(r.Context(), firstName, lastName)
	if err != nil {
		log.Printf("search: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, users)
}

// authorize validates the bearer token, writing the error response itself.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) bool {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing or malformed Authorization header")
		return false
	}
	if _, err := h.store.UserIDByToken(r.Context(), token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return false
		}
		log.Printf("auth: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
	return true
}
