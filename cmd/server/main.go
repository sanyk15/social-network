package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"social-network/internal/cache"
	"social-network/internal/config"
	"social-network/internal/feed"
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

	rdb, err := cache.NewRedis(ctx, cfg.RedisURL)
	if err != nil {
		log.Fatalf("connect to redis: %v", err)
	}
	defer rdb.Close()

	mode := feed.Mode(cfg.FeedFanoutMode)
	if mode != feed.ModeQueue {
		mode = feed.ModeSync
	}
	var queue *feed.Queue
	if mode == feed.ModeQueue {
		queue = feed.NewQueue(rdb, "sn")
	}

	feeds := feed.NewService(st, rdb, queue, feed.Config{
		Size:       cfg.FeedSize,
		FeedTTL:    cfg.FeedCacheTTL,
		FriendsTTL: cfg.FriendsCacheTTL,
		PostTTL:    cfg.PostCacheTTL,
		IncludeOwn: cfg.FeedIncludeOwn,
		Mode:       mode,
	})

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	feeds.StartWorkers(runCtx, cfg.FeedWorkers)

	h := httpapi.New(st, feeds, cfg.TokenTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", h.Login)
	mux.HandleFunc("POST /user/register", h.Register)
	mux.HandleFunc("GET /user/get/{id}", h.GetUser)
	mux.HandleFunc("GET /user/search", h.Search)

	// Дружба: методы из задания (/friend/add, /friend/delete) и из
	// спецификации курса (/friend/set/{user_id}, /friend/delete/{user_id}).
	mux.HandleFunc("POST /friend/add", h.FriendAdd)
	mux.HandleFunc("POST /friend/delete", h.FriendDelete)
	mux.HandleFunc("PUT /friend/set/{user_id}", h.FriendAdd)
	mux.HandleFunc("PUT /friend/delete/{user_id}", h.FriendDelete)

	// Посты: формы из задания и из спецификации.
	mux.HandleFunc("POST /post/create", h.CreatePost)
	mux.HandleFunc("POST /post/update", h.UpdatePost)
	mux.HandleFunc("PUT /post/update", h.UpdatePost)
	mux.HandleFunc("POST /post/delete", h.DeletePost)
	mux.HandleFunc("PUT /post/delete/{id}", h.DeletePost)
	mux.HandleFunc("GET /post/get", h.GetPost)
	mux.HandleFunc("GET /post/get/{id}", h.GetPost)
	mux.HandleFunc("GET /post/feed", h.Feed)

	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /readyz", h.Ready)
	if cfg.EnableTestAPI {
		mux.HandleFunc("POST /test/write", h.TestWrite)
		mux.HandleFunc("GET /test/write/count", h.TestWriteCount)
		mux.HandleFunc("GET /test/replication", h.TestReplication)

		// Перестройка кэша из СУБД и диагностика кэша/очереди.
		mux.HandleFunc("POST /admin/feed/rebuild", h.RebuildFeed)
		mux.HandleFunc("GET /admin/feed/status", h.FeedStatus)
		mux.HandleFunc("GET /admin/feed/stats", h.FeedStats)
	}

	var handler http.Handler = mux
	if cfg.LogRequests {
		handler = httpapi.LogRequests(mux)
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: handler,
	}

	go func() {
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("listening on %s (feed fanout mode %s, cache size %d)", cfg.HTTPAddr, mode, cfg.FeedSize)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
