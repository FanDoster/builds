package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

func newTestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	s := &Server{DB: database, BuildCh: make(chan *models.Build, 10)}
	mux := http.NewServeMux()
	s.RegisterRoutes(mux)
	return s, mux
}

func createProject(t *testing.T, s *Server, p models.Project) *models.Project {
	t.Helper()
	if err := s.DB.CreateProject(&p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &p
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// --- Projects ---

func TestCreateProjectValidation(t *testing.T) {
	_, mux := newTestServer(t)

	w := doJSON(t, mux, "POST", "/api/projects", map[string]string{"name": "x"})
	if w.Code != 400 {
		t.Errorf("missing fields: got %d, want 400", w.Code)
	}

	w = doJSON(t, mux, "POST", "/api/projects", map[string]string{
		"name": "app", "repo_url": "https://github.com/u/app", "image_name": "app",
	})
	if w.Code != 201 {
		t.Fatalf("create: got %d, want 201: %s", w.Code, w.Body)
	}
	var created models.Project
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Branch != "main" || created.DockerfilePath != "Dockerfile" {
		t.Errorf("defaults not applied: %+v", created)
	}

	// Duplicate name
	w = doJSON(t, mux, "POST", "/api/projects", map[string]string{
		"name": "app", "repo_url": "https://github.com/u/app", "image_name": "app",
	})
	if w.Code != 409 {
		t.Errorf("duplicate: got %d, want 409", w.Code)
	}
}

func TestProjectResponsesOmitSecrets(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		WebhookSecret: "whsec", CloneToken: "tok",
	})

	for _, path := range []string{"/api/projects", fmt.Sprintf("/api/projects/%d", p.ID)} {
		w := doJSON(t, mux, "GET", path, nil)
		if w.Code != 200 {
			t.Fatalf("GET %s: got %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "whsec") || strings.Contains(w.Body.String(), "tok") {
			t.Errorf("GET %s leaks secrets: %s", path, w.Body)
		}
	}
}

func TestUpdateProjectPartialAndClear(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		DeployComposePath: "/srv/compose.yml", DeployServiceName: "web",
		WebhookSecret: "whsec", CloneToken: "tok",
	})
	path := fmt.Sprintf("/api/projects/%d", p.ID)

	// Omitted fields must be preserved.
	w := doJSON(t, mux, "PUT", path, map[string]string{"branch": "develop"})
	if w.Code != 200 {
		t.Fatalf("update: got %d: %s", w.Code, w.Body)
	}
	got, _ := s.DB.GetProject(p.ID)
	if got.Branch != "develop" {
		t.Errorf("branch not updated: %q", got.Branch)
	}
	if got.DeployComposePath != "/srv/compose.yml" || got.WebhookSecret != "whsec" || got.CloneToken != "tok" {
		t.Errorf("omitted fields were modified: %+v", got)
	}

	// Explicit empty strings clear the deploy config.
	w = doJSON(t, mux, "PUT", path, map[string]string{
		"deploy_compose_path": "", "deploy_service_name": "",
	})
	if w.Code != 200 {
		t.Fatalf("clear: got %d: %s", w.Code, w.Body)
	}
	got, _ = s.DB.GetProject(p.ID)
	if got.DeployComposePath != "" || got.DeployServiceName != "" {
		t.Errorf("deploy config not cleared: %+v", got)
	}

	// Required fields cannot be cleared.
	w = doJSON(t, mux, "PUT", path, map[string]string{"name": ""})
	if w.Code != 400 {
		t.Errorf("clearing name: got %d, want 400", w.Code)
	}
}

// --- Builds ---

func TestTriggerBuild(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 201 {
		t.Fatalf("trigger: got %d: %s", w.Code, w.Body)
	}
	select {
	case b := <-s.BuildCh:
		if b.ProjectID != p.ID || b.Status != models.StatusPending {
			t.Errorf("queued build wrong: %+v", b)
		}
	default:
		t.Error("build was not queued")
	}

	w = doJSON(t, mux, "POST", "/api/projects/999/build", nil)
	if w.Code != 404 {
		t.Errorf("unknown project: got %d, want 404", w.Code)
	}
}

func TestTriggerBuildQueueFull(t *testing.T) {
	s, mux := newTestServer(t)
	s.BuildCh = make(chan *models.Build) // unbuffered, nothing reading = always full
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	w := doJSON(t, mux, "POST", fmt.Sprintf("/api/projects/%d/build", p.ID), nil)
	if w.Code != 503 {
		t.Fatalf("full queue: got %d, want 503: %s", w.Code, w.Body)
	}
	// The created build must be marked failed, not left pending forever.
	builds, _ := s.DB.ListBuildsByProject(p.ID, 0)
	if len(builds) != 1 || builds[0].Status != models.StatusFailed {
		t.Errorf("build not marked failed: %+v", builds)
	}
}

// --- Webhook ---

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushPayload(t *testing.T, ref, cloneURL, sha, msg string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]interface{}{
		"ref":         ref,
		"repository":  map[string]string{"clone_url": cloneURL, "full_name": "u/app"},
		"head_commit": map[string]string{"id": sha, "message": msg},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postWebhook(mux *http.ServeMux, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "push")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestWebhookSignature(t *testing.T) {
	s, mux := newTestServer(t)
	p := createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app.git", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app", WebhookSecret: "s3cret",
	})

	body := pushPayload(t, "refs/heads/main", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "fix stuff")

	// Missing signature → rejected
	w := postWebhook(mux, body, nil)
	if w.Code != 403 {
		t.Errorf("missing signature: got %d, want 403: %s", w.Code, w.Body)
	}

	// Wrong signature → rejected
	w = postWebhook(mux, body, map[string]string{"X-Hub-Signature-256": sign("wrong", body)})
	if w.Code != 403 {
		t.Errorf("bad signature: got %d, want 403: %s", w.Code, w.Body)
	}
	if builds, _ := s.DB.ListBuildsByProject(p.ID, 0); len(builds) != 0 {
		t.Errorf("rejected webhook still created builds: %+v", builds)
	}

	// Valid signature → queued
	w = postWebhook(mux, body, map[string]string{"X-Hub-Signature-256": sign("s3cret", body)})
	if w.Code != 200 {
		t.Fatalf("valid signature: got %d: %s", w.Code, w.Body)
	}
	builds, _ := s.DB.ListBuildsByProject(p.ID, 0)
	if len(builds) != 1 {
		t.Fatalf("expected 1 build, got %d", len(builds))
	}
	if builds[0].CommitSHA != "0123456789ab" {
		t.Errorf("sha not truncated to 12: %q", builds[0].CommitSHA)
	}
	select {
	case <-s.BuildCh:
	default:
		t.Error("build not queued on channel")
	}
}

func TestWebhookNoSecretConfigured(t *testing.T) {
	s, mux := newTestServer(t)
	createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	body := pushPayload(t, "refs/heads/main", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "hello")
	w := postWebhook(mux, body, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "queued") {
		t.Errorf("no-secret project should accept push: got %d: %s", w.Code, w.Body)
	}
}

func TestWebhookNoHeadCommit(t *testing.T) {
	_, mux := newTestServer(t)

	// Branch deletion pushes have "head_commit": null — this used to panic.
	body := []byte(`{"ref":"refs/heads/main","repository":{"clone_url":"https://github.com/u/app.git"},"head_commit":null,"deleted":true}`)
	w := postWebhook(mux, body, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "no head_commit") {
		t.Errorf("null head_commit: got %d: %s", w.Code, w.Body)
	}

	// Short SHA must not panic either.
	body = pushPayload(t, "refs/heads/main", "https://github.com/u/app.git", "abc", "short")
	w = postWebhook(mux, body, nil)
	if w.Code != 200 {
		t.Errorf("short sha: got %d: %s", w.Code, w.Body)
	}
}

func TestWebhookIgnoresNonPushAndOtherBranch(t *testing.T) {
	s, mux := newTestServer(t)
	createProject(t, s, models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	})

	body := pushPayload(t, "refs/heads/feature", "https://github.com/u/app.git",
		"0123456789abcdef0123456789abcdef01234567", "wip")
	w := postWebhook(mux, body, nil)
	if !strings.Contains(w.Body.String(), "no matching project") {
		t.Errorf("other branch should be ignored: %s", w.Body)
	}

	req := httptest.NewRequest("POST", "/api/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "ping")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("ping event should be ignored: %s", rec.Body)
	}
	if builds, _ := s.DB.ListRecentBuilds(10); len(builds) != 0 {
		t.Errorf("ignored events created builds: %+v", builds)
	}
}

// --- Helpers ---

func TestRepoURLMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://github.com/u/app", "https://github.com/u/app.git", true},
		{"http://github.com/u/app.git", "https://github.com/u/app", true},
		{"https://github.com/u/app", "https://github.com/u/other.git", false},
	}
	for _, c := range cases {
		if got := repoURLMatch(c.a, c.b); got != c.want {
			t.Errorf("repoURLMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 100, "short"},
		{"first line\nsecond line", 100, "first line"},
		{strings.Repeat("a", 10), 8, "aaaaa..."},
		// Multi-byte runes must not be split mid-character.
		{strings.Repeat("é", 10), 8, "ééééé..."},
		{strings.Repeat("日", 10), 8, "日日日日日..."},
		{"héllo", 2, "hé"},
	}
	for _, c := range cases {
		got := truncate(c.in, c.n)
		if got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestValidSignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	if !validSignature("secret", body, sign("secret", body)) {
		t.Error("valid signature rejected")
	}
	if validSignature("secret", body, sign("other", body)) {
		t.Error("invalid signature accepted")
	}
	if validSignature("secret", body, "") {
		t.Error("empty signature accepted")
	}
}
