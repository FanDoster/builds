package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

func setup(t *testing.T) (*db.DB, *http.ServeMux) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	mux := http.NewServeMux()
	New(database, "").RegisterRoutes(mux)
	return database, mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// Renders every page with a finished build, which used to break the project
// page ({{.FinishedAt.Sub .StartedAt}} on pointer receivers + %.0f on a
// Duration produced a template error mid-render).
func TestPagesRenderWithFinishedBuild(t *testing.T) {
	database, mux := setup(t)

	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
	}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc123", CommitMessage: "msg"}
	if err := database.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	database.UpdateBuildStatus(b.ID, models.StatusRunning, "started\n")
	database.UpdateBuildStatus(b.ID, models.StatusSuccess, "started\ndone\n")

	for _, path := range []string{"/", fmt.Sprintf("/projects/%d", p.ID), fmt.Sprintf("/builds/%d", b.ID)} {
		w := get(t, mux, path)
		if w.Code != 200 {
			t.Errorf("GET %s: got %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "%!") {
			t.Errorf("GET %s: template formatting error in output:\n%s", path, w.Body)
		}
	}

	// Project page must show a real duration, not an empty/broken cell.
	w := get(t, mux, fmt.Sprintf("/projects/%d", p.ID))
	if !strings.Contains(w.Body.String(), "s</td>") {
		t.Errorf("project page missing build duration:\n%s", w.Body)
	}
}

func TestNotFoundAndBadIDs(t *testing.T) {
	_, mux := setup(t)
	if w := get(t, mux, "/projects/999"); w.Code != 404 {
		t.Errorf("missing project: got %d, want 404", w.Code)
	}
	if w := get(t, mux, "/builds/abc"); w.Code != 400 {
		t.Errorf("bad build id: got %d, want 400", w.Code)
	}
}
