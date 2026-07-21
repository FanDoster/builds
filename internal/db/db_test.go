package db

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestFailStaleRunningPreservesUnknownFinishTime(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	mk := func() *models.Build {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		if ok, err := d.ClaimBuild(b.ID); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return b
	}
	current, stale := mk(), mk()

	failed, err := d.FailStaleRunning(current.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0] != stale.ID {
		t.Fatalf("failed ids = %v, want [%d]", failed, stale.ID)
	}

	got, _ := d.GetBuild(stale.ID)
	if got.Status != models.StatusFailed {
		t.Errorf("stale status = %s, want failed", got.Status)
	}
	if got.FinishedAt != nil {
		t.Errorf("stale build got a fabricated finished_at: %v (poisons history durations)", got.FinishedAt)
	}
	if !strings.Contains(got.Log, "interrupted by server restart") {
		t.Errorf("missing sweep note in log: %q", got.Log)
	}
	if got.Duration() != "" {
		t.Errorf("duration should be unknown, got %q", got.Duration())
	}

	cur, _ := d.GetBuild(current.ID)
	if cur.Status != models.StatusRunning {
		t.Errorf("current build was swept: %s", cur.Status)
	}
}

func TestRepairInterruptedDurations(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
	if err := d.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	// Simulate a row swept by the OLD code: failed with the marker AND a
	// bogus finished_at stamped at restart time.
	d.ClaimBuild(b.ID)
	d.UpdateBuildStatus(b.ID, models.StatusFailed, "some log\n[ERROR] Build interrupted by server restart\n")
	if got, _ := d.GetBuild(b.ID); got.FinishedAt == nil {
		t.Fatal("precondition: finished_at should be set by old-style sweep")
	}

	n, err := d.RepairInterruptedDurations()
	if err != nil || n != 1 {
		t.Fatalf("repair: n=%d err=%v", n, err)
	}
	got, _ := d.GetBuild(b.ID)
	if got.FinishedAt != nil {
		t.Errorf("finished_at not cleared: %v", got.FinishedAt)
	}
	// Idempotent.
	if n, _ := d.RepairInterruptedDurations(); n != 0 {
		t.Errorf("second repair touched %d rows, want 0", n)
	}
}

func TestExpectedDuration(t *testing.T) {
	d := openTestDB(t)
	p := newProject(t, d)

	if _, ok := d.ExpectedDuration(p.ID); ok {
		t.Error("no history should mean no estimate")
	}

	// Seed successful builds with controlled 30s/60s durations.
	base := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	for i, secs := range []int{30, 60} {
		b := &models.Build{ProjectID: p.ID, Status: models.StatusPending}
		if err := d.CreateBuild(b); err != nil {
			t.Fatal(err)
		}
		start := base.Add(time.Duration(i) * time.Hour)
		if _, err := d.conn.Exec(
			`UPDATE builds SET status=?, started_at=?, finished_at=? WHERE id=?`,
			models.StatusSuccess, start, start.Add(time.Duration(secs)*time.Second), b.ID,
		); err != nil {
			t.Fatal(err)
		}
	}

	got, ok := d.ExpectedDuration(p.ID)
	if !ok || got != 45*time.Second {
		t.Errorf("expected duration = %v ok=%v, want 45s", got, ok)
	}
}
