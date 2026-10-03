package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,64}$`)

var validGenders = map[string]bool{
	"male":   true,
	"female": true,
	"other":  true,
}

func validateRegister(req *registerRequest) error {
	if !usernameRe.MatchString(req.Username) {
		return errors.New("username must be 3-64 chars of [a-zA-Z0-9_.-]")
	}
	if len(req.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if req.FirstName == "" || req.LastName == "" || req.City == "" {
		return errors.New("first_name, last_name and city are required")
	}
	if _, err := time.Parse("2006-01-02", req.BirthDate); err != nil {
		return errors.New("birth_date must be in YYYY-MM-DD format")
	}
	if !validGenders[req.Gender] {
		return errors.New("gender must be one of: male, female, other")
	}
	return nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	t := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	return t, t != ""
}
