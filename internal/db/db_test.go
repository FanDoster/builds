package db

import (
	"path/filepath"
	"testing"

	"github.com/FanDoster/builds/internal/models"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newProject(t *testing.T, d *DB) *models.Project {
	t.Helper()
	p := &models.Project{
		Name: "app", RepoURL: "https://github.com/u/app", Branch: "main",
		DockerfilePath: "Dockerfile", ImageName: "app",
		WebhookSecret: "whsec", CloneToken: "tok",
	}
	if err := d.CreateProject(p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func TestProjectRoundtrip(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	got, err := d.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "app" || got.WebhookSecret != "whsec" || got.CloneToken != "tok" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}

	got.Branch = "develop"
	if err := d.UpdateProject(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := d.GetProject(p.ID)
	if got2.Branch != "develop" {
		t.Errorf("update not persisted: %+v", got2)
	}

	// ListProjects intentionally omits secret columns.
	list, err := d.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].WebhookSecret != "" || list[0].CloneToken != "" {
		t.Errorf("ListProjects should not return secrets: %+v", list)
	}

	if err := d.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := d.GetProject(p.ID); err == nil {
		t.Error("project still present after delete")
	}
}

func TestBuildLifecycle(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending, CommitSHA: "abc123", CommitMessage: "msg"}
	if err := d.CreateBuild(b); err != nil {
		t.Fatalf("create build: %v", err)
	}

	if err := d.UpdateBuildStatus(b.ID, models.StatusRunning, "started\n"); err != nil {
		t.Fatalf("running: %v", err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.Status != models.StatusRunning || got.StartedAt == nil || got.FinishedAt != nil {
		t.Errorf("running state wrong: %+v", got)
	}

	if err := d.AppendBuildLog(b.ID, "line two\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, _ = d.GetBuild(b.ID)
	if got.Log != "started\nline two\n" {
		t.Errorf("log append mismatch: %q", got.Log)
	}

	if err := d.UpdateBuildStatus(b.ID, models.StatusSuccess, "full log"); err != nil {
		t.Fatalf("success: %v", err)
	}
	got, _ = d.GetBuild(b.ID)
	if got.Status != models.StatusSuccess || got.StartedAt == nil || got.FinishedAt == nil {
		t.Errorf("finished state wrong: %+v", got)
	}
	if got.ProjectName != "app" {
		t.Errorf("project name not joined: %q", got.ProjectName)
	}
}

func TestListBuildsByStatus(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	for _, st := range []models.BuildStatus{models.StatusPending, models.StatusPending, models.StatusRunning} {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if st != models.StatusPending {
			if err := d.UpdateBuildStatus(b.ID, st, ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	pending, err := d.ListBuildsByStatus(models.StatusPending)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("pending: got %d, want 2", len(pending))
	}
	running, err := d.ListBuildsByStatus(models.StatusRunning)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 1 {
		t.Errorf("running: got %d, want 1", len(running))
	}
}

func TestDeleteProjectCascadesBuilds(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := d.GetBuild(b.ID); err == nil {
		t.Error("build survived project deletion (ON DELETE CASCADE not effective)")
	}
}
