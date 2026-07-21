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

	"github.com/FanDoster/builds/internal/api"
	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
	"github.com/FanDoster/builds/internal/runner"
	"github.com/FanDoster/builds/internal/web"
)

func main() {
	addr := getEnv("BUILDS_ADDR", ":8080")
	dbPath := getEnv("BUILDS_DB", "/var/lib/builds/builds.db")
	basePath := getEnv("BUILDS_BASE_PATH", "")

	// Open database
	database, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()
	log.Printf("Database opened: %s", dbPath)

	// Build job queue (buffered channel)
	buildCh := make(chan *models.Build, 100)

	// Recover builds orphaned by a previous shutdown before accepting new work.
	recoverOrphanedBuilds(database, buildCh)

	// Start runner
	r := runner.New(database, buildCh)
	if v := os.Getenv("BUILDS_BUILD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("Invalid BUILDS_BUILD_TIMEOUT %q: %v", v, err)
		}
		r.Timeout = d
	}
	r.Start()
	log.Printf("Runner started (build timeout: %s)", r.Timeout)

	// Wire up HTTP
	mux := http.NewServeMux()

	// API
	apiServer := &api.Server{DB: database, BuildCh: buildCh, BasePath: basePath}
	apiServer.RegisterRoutes(mux)

	// Web UI
	webHandler := web.New(database, basePath)
	webHandler.RegisterRoutes(mux)

	server := &http.Server{Addr: addr, Handler: mux}

	// Shut down cleanly on SIGINT/SIGTERM so in-flight builds are not left
	// stuck in "running".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("Builds server listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server failed: %v", err)
	}

	// Cancel any in-flight build (it is marked failed with a shutdown note)
	// and wait for the worker to exit.
	r.Stop()
	log.Println("Shutdown complete")
}

// recoverOrphanedBuilds marks builds that were mid-flight during a previous
// crash/restart as failed, and re-queues builds that never started.
func recoverOrphanedBuilds(database *db.DB, buildCh chan *models.Build) {
	interrupted, err := database.ListBuildsByStatus(models.StatusRunning)
	if err != nil {
		log.Printf("Recovery: failed to list running builds: %v", err)
	}
	for _, b := range interrupted {
		database.UpdateBuildStatus(b.ID, models.StatusFailed, b.Log+"\n[ERROR] Build interrupted by server restart\n")
		log.Printf("Recovery: marked build %d failed (interrupted by restart)", b.ID)
	}

	pending, err := database.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		log.Printf("Recovery: failed to list pending builds: %v", err)
	}
	for i := range pending {
		b := &pending[i]
		select {
		case buildCh <- b:
			log.Printf("Recovery: re-queued pending build %d", b.ID)
		default:
			database.UpdateBuildStatus(b.ID, models.StatusFailed, b.Log+"\n[ERROR] Build not re-queued after restart: queue is full\n")
			log.Printf("Recovery: queue full, marked pending build %d failed", b.ID)
		}
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
