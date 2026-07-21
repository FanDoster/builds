package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

type Server struct {
	DB       *db.DB
	BuildCh  chan *models.Build
	BasePath string // e.g. "/builds"
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)

	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)
	mux.HandleFunc("PUT /api/projects/{id}", s.handleUpdateProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)

	mux.HandleFunc("POST /api/projects/{id}/build", s.handleTriggerBuild)
	mux.HandleFunc("GET /api/projects/{id}/builds", s.handleListProjectBuilds)
	mux.HandleFunc("GET /api/builds", s.handleListRecentBuilds)
	mux.HandleFunc("GET /api/builds/{id}", s.handleGetBuild)

	mux.HandleFunc("POST /api/webhook/github", s.handleGitHubWebhook)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// --- Projects ---

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if projects == nil {
		projects = []models.Project{}
	}
	writeJSON(w, 200, projects)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p models.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if p.Name == "" || p.RepoURL == "" || p.ImageName == "" {
		writeError(w, 400, "name, repo_url, image_name are required")
		return
	}
	if p.Branch == "" {
		p.Branch = "main"
	}
	if p.DockerfilePath == "" {
		p.DockerfilePath = "Dockerfile"
	}

	if err := s.DB.CreateProject(&p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, 409, "project name already exists")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	// Re-fetch to get secrets cleared from response
	created, _ := s.DB.GetProject(p.ID)
	writeJSON(w, 201, created)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	p, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	writeJSON(w, 200, p)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	existing, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}

	var updates models.Project
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	// Merge updates into existing
	if updates.Name != "" {
		existing.Name = updates.Name
	}
	if updates.RepoURL != "" {
		existing.RepoURL = updates.RepoURL
	}
	if updates.Branch != "" {
		existing.Branch = updates.Branch
	}
	if updates.DockerfilePath != "" {
		existing.DockerfilePath = updates.DockerfilePath
	}
	if updates.ImageName != "" {
		existing.ImageName = updates.ImageName
	}
	if updates.DeployComposePath != "" || r.FormValue("clear_compose") == "true" {
		existing.DeployComposePath = updates.DeployComposePath
	}
	if updates.DeployServiceName != "" || r.FormValue("clear_service") == "true" {
		existing.DeployServiceName = updates.DeployServiceName
	}
	if updates.WebhookSecret != "" {
		existing.WebhookSecret = updates.WebhookSecret
	}
	if updates.CloneToken != "" {
		existing.CloneToken = updates.CloneToken
	}

	if err := s.DB.UpdateProject(existing); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Re-fetch without secrets
	updated, _ := s.DB.GetProject(id)
	writeJSON(w, 200, updated)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	if err := s.DB.DeleteProject(id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// --- Builds ---

func (s *Server) handleTriggerBuild(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	project, err := s.DB.GetProject(id)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}

	build := &models.Build{
		ProjectID:     project.ID,
		Status:        models.StatusPending,
		CommitSHA:     "manual",
		CommitMessage: "Manual trigger",
	}
	if err := s.DB.CreateBuild(build); err != nil {
		writeError(w, 500, err.Error())
		return
	}

	// Queue the build
	s.BuildCh <- build

	writeJSON(w, 201, build)
}

func (s *Server) handleListProjectBuilds(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	builds, err := s.DB.ListBuildsByProject(id, 0)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if builds == nil {
		builds = []models.Build{}
	}
	writeJSON(w, 200, builds)
}

func (s *Server) handleListRecentBuilds(w http.ResponseWriter, r *http.Request) {
	builds, err := s.DB.ListRecentBuilds(30)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if builds == nil {
		builds = []models.Build{}
	}
	writeJSON(w, 200, builds)
}

func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid id")
		return
	}
	build, err := s.DB.GetBuild(id)
	if err != nil {
		writeError(w, 404, "build not found")
		return
	}
	writeJSON(w, 200, build)
}

// --- GitHub Webhook ---

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	// Parse the event
	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": fmt.Sprintf("event=%s, only push is handled", event)})
		return
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"head_commit"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, 400, "invalid payload")
		return
	}

	// Find matching project(s) by repo URL
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	var matched []models.Project
	for _, p := range projects {
		if p.Branch == branch && repoURLMatch(p.RepoURL, payload.Repository.CloneURL) {
			// TODO: validate X-Hub-Signature-256 against p.WebhookSecret
			matched = append(matched, p)
		}
	}

	if len(matched) == 0 {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "no matching project"})
		return
	}

	var created []int64
	for _, project := range matched {
		build := &models.Build{
			ProjectID:     project.ID,
			Status:        models.StatusPending,
			CommitSHA:     payload.HeadCommit.ID[:12],
			CommitMessage: truncate(payload.HeadCommit.Message, 100),
		}
		if err := s.DB.CreateBuild(build); err != nil {
			continue
		}
		created = append(created, build.ID)
		s.BuildCh <- build
	}

	writeJSON(w, 200, map[string]interface{}{
		"status":  "queued",
		"builds":  created,
		"project": matched[0].Name,
	})
}

func repoURLMatch(configured, webhook string) bool {
	// Normalize: strip .git suffix, strip https:// prefix
	norm := func(s string) string {
		s = strings.TrimPrefix(s, "https://")
		s = strings.TrimPrefix(s, "http://")
		s = strings.TrimSuffix(s, ".git")
		return s
	}
	return norm(configured) == norm(webhook)
}

func truncate(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}
