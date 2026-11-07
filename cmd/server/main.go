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
	st, err := store.New(ctx, store.Config{
		MasterURL:    cfg.DatabaseURL,
		ReplicaURLs:  cfg.DatabaseReplicaURLs,
		MaxConns:     cfg.DBMaxConns,
		ReadFallback: cfg.ReadFallbackToMaster,
	})
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
	mux.HandleFunc("GET /user/search", h.Search)
	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /readyz", h.Ready)
	if cfg.EnableTestAPI {
		mux.HandleFunc("POST /test/write", h.TestWrite)
		mux.HandleFunc("GET /test/write/count", h.TestWriteCount)
		mux.HandleFunc("GET /test/replication", h.TestReplication)
	}

	var handler http.Handler = mux
	if cfg.LogRequests {
		handler = httpapi.LogRequests(mux)
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
	}

	log.Printf("listening on %s", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
