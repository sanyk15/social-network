package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	"social-network/internal/config"
	"social-network/internal/httpapi"
	"social-network/internal/store"
)

func main() {
	cfg := config.Load()

	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	h := httpapi.New(st, cfg.TokenTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", h.Login)
	mux.HandleFunc("POST /user/register", h.Register)
	mux.HandleFunc("GET /user/get/{id}", h.GetUser)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.LogRequests(mux),
	}

	log.Printf("listening on %s", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
