package main

import (
	"log"
	"net/http"
	"os"

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

	// Start runner
	r := runner.New(database, buildCh)
	r.Start()
	log.Println("Runner started")

	// Wire up HTTP
	mux := http.NewServeMux()

	// API
	apiServer := &api.Server{DB: database, BuildCh: buildCh, BasePath: basePath}
	apiServer.RegisterRoutes(mux)

	// Web UI
	webHandler := web.New(database, basePath)
	webHandler.RegisterRoutes(mux)

	log.Printf("Builds server listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
