package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

// maxWebhookBody caps webhook payload reads (GitHub push payloads are far smaller).
const maxWebhookBody = 1 << 20

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
	for i := range projects {
		projects[i].Sanitize()
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
	created.Sanitize()
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
	p.Sanitize()
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

	// Pointer fields distinguish "not provided" (nil, keep current value)
	// from "provided as empty" (clear the value where that is allowed).
	var updates struct {
		Name              *string `json:"name"`
		RepoURL           *string `json:"repo_url"`
		Branch            *string `json:"branch"`
		DockerfilePath    *string `json:"dockerfile_path"`
		ImageName         *string `json:"image_name"`
		DeployComposePath *string `json:"deploy_compose_path"`
		DeployServiceName *string `json:"deploy_service_name"`
		WebhookSecret     *string `json:"webhook_secret"`
		CloneToken        *string `json:"clone_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}

	// Required fields may be updated but not cleared.
	for field, v := range map[string]*string{
		"name": updates.Name, "repo_url": updates.RepoURL, "branch": updates.Branch,
		"dockerfile_path": updates.DockerfilePath, "image_name": updates.ImageName,
	} {
		if v != nil && *v == "" {
			writeError(w, 400, field+" cannot be empty")
			return
		}
	}

	setIf := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	setIf(&existing.Name, updates.Name)
	setIf(&existing.RepoURL, updates.RepoURL)
	setIf(&existing.Branch, updates.Branch)
	setIf(&existing.DockerfilePath, updates.DockerfilePath)
	setIf(&existing.ImageName, updates.ImageName)
	setIf(&existing.DeployComposePath, updates.DeployComposePath)
	setIf(&existing.DeployServiceName, updates.DeployServiceName)
	setIf(&existing.WebhookSecret, updates.WebhookSecret)
	setIf(&existing.CloneToken, updates.CloneToken)

	if err := s.DB.UpdateProject(existing); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Re-fetch without secrets
	updated, _ := s.DB.GetProject(id)
	updated.Sanitize()
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

	if !s.enqueue(build) {
		writeError(w, 503, "build queue is full, try again later")
		return
	}

	writeJSON(w, 201, build)
}

// enqueue attempts a non-blocking send to the build channel. On a full queue
// it marks the build failed rather than blocking the HTTP handler forever.
func (s *Server) enqueue(build *models.Build) bool {
	select {
	case s.BuildCh <- build:
		return true
	default:
		s.DB.UpdateBuildStatus(build.ID, models.StatusFailed, "Build not started: queue is full\n")
		return false
	}
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

	// Keep the raw body: signature validation is HMAC over the exact bytes.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, 400, "failed to read payload")
		return
	}

	var payload struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"head_commit"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, 400, "invalid payload")
		return
	}

	// Branch deletions and some tag pushes have no head_commit.
	if payload.HeadCommit == nil || payload.HeadCommit.ID == "" {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "no head_commit in payload"})
		return
	}

	// Find matching project(s) by repo URL
	projects, err := s.DB.ListProjects()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
	var matched []models.Project
	rejected := 0
	for _, p := range projects {
		if p.Branch != branch || !repoURLMatch(p.RepoURL, payload.Repository.CloneURL) {
			continue
		}
		// ListProjects omits secrets; fetch the full row for validation.
		full, err := s.DB.GetProject(p.ID)
		if err != nil {
			continue
		}
		if full.WebhookSecret != "" && !validSignature(full.WebhookSecret, body, signature) {
			rejected++
			continue
		}
		matched = append(matched, *full)
	}

	if len(matched) == 0 {
		if rejected > 0 {
			writeError(w, 403, "invalid webhook signature")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "no matching project"})
		return
	}

	sha := payload.HeadCommit.ID
	if len(sha) > 12 {
		sha = sha[:12]
	}

	var created []int64
	for _, project := range matched {
		build := &models.Build{
			ProjectID:     project.ID,
			Status:        models.StatusPending,
			CommitSHA:     sha,
			CommitMessage: truncate(payload.HeadCommit.Message, 100),
		}
		if err := s.DB.CreateBuild(build); err != nil {
			continue
		}
		if !s.enqueue(build) {
			continue
		}
		created = append(created, build.ID)
	}

	writeJSON(w, 200, map[string]interface{}{
		"status":  "queued",
		"builds":  created,
		"project": matched[0].Name,
	})
}

// validSignature checks a GitHub X-Hub-Signature-256 header (constant-time).
func validSignature(secret string, body []byte, sigHeader string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sigHeader))
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

// truncate returns the first line of s, capped at n runes (not bytes, so
// multi-byte characters are never split).
func truncate(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return string(runes[:n])
	}
	return string(runes[:n-3]) + "..."
}
