package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/logbus"
	"github.com/FanDoster/builds/internal/models"
)

var (
	sinkDBOnce sync.Once
	sinkDB     *db.DB
	sinkProjID int64
)

// sinkFixtures shares one DB across all sink tests (opening SQLite per
// iteration dominates test time); each call gets a fresh build row.
func sinkFixtures(t *testing.T) (*db.DB, *logbus.Bus, int64) {
	t.Helper()
	sinkDBOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sinktest-")
		if err != nil {
			t.Fatal(err)
		}
		sinkDB, err = db.Open(filepath.Join(dir, "test.db"))
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		p := &models.Project{Name: "p", RepoURL: "https://x", Branch: "main", DockerfilePath: "Dockerfile", ImageName: "p"}
		if err := sinkDB.CreateProject(p); err != nil {
			t.Fatal(err)
		}
		sinkProjID = p.ID
	})
	b := &models.Build{ProjectID: sinkProjID, Status: models.StatusPending}
	if err := sinkDB.CreateBuild(b); err != nil {
		t.Fatal(err)
	}
	return sinkDB, logbus.New(), b.ID
}

// A secret split at EVERY byte boundary across two Writes must never reach
// the bus or the DB.
func TestSinkSecretStraddlesWriteBoundary(t *testing.T) {
	const secret = "tok-SECRET-123"
	line := "fetch https://" + secret + "@github.com/u/r failed\n"

	for cut := 0; cut <= len(line); cut++ {
		database, bus, buildID := sinkFixtures(t)
		sink := newLogSink(buildID, secret, database, bus)

		sink.Write([]byte(line[:cut]))
		sink.Write([]byte(line[cut:]))
		sink.Close()

		tail, _, ok := bus.LogTail(buildID, 0)
		if !ok {
			t.Fatalf("cut=%d: no bus data", cut)
		}
		if strings.Contains(string(tail), secret) {
			t.Fatalf("cut=%d: secret leaked to bus: %q", cut, tail)
		}
		if !strings.Contains(string(tail), "***") {
			t.Fatalf("cut=%d: secret not masked: %q", cut, tail)
		}
		build, err := database.GetBuild(buildID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(build.Log, secret) {
			t.Fatalf("cut=%d: secret leaked to DB: %q", cut, build.Log)
		}
		if build.Log != string(tail) {
			t.Fatalf("cut=%d: DB log %q != bus buffer %q", cut, build.Log, tail)
		}
	}
}

// A secret inside an overlong unterminated line (forced drain path) must
// still be masked thanks to the holdback window.
func TestSinkSecretInForcedDrain(t *testing.T) {
	const secret = "tok-SECRET-123"
	database, bus, buildID := sinkFixtures(t)
	sink := newLogSink(buildID, secret, database, bus)

	// One giant line with the secret placed right around the drain cut.
	huge := strings.Repeat("x", sinkMaxHold-4) + secret + strings.Repeat("y", 200)
	for i := 0; i < len(huge); i += 100 {
		end := i + 100
		if end > len(huge) {
			end = len(huge)
		}
		sink.Write([]byte(huge[i:end]))
	}
	sink.Close()

	tail, _, _ := bus.LogTail(buildID, 0)
	if strings.Contains(string(tail), secret) {
		t.Fatalf("secret leaked through forced drain")
	}
}

func TestSinkCRSegmentsAndPartialLine(t *testing.T) {
	database, bus, buildID := sinkFixtures(t)
	sink := newLogSink(buildID, "", database, bus)

	sink.Write([]byte("Progress 10%\rProgress 20%\rdone\npartial"))
	tail, _, _ := bus.LogTail(buildID, 0)
	if string(tail) != "Progress 10%\rProgress 20%\rdone\n" {
		t.Errorf("emitted = %q, want CR/NL-terminated segments only", tail)
	}

	// The partial line is held until more input or Close.
	sink.Write([]byte(" now finished\n"))
	tail, _, _ = bus.LogTail(buildID, 0)
	if !strings.HasSuffix(string(tail), "partial now finished\n") {
		t.Errorf("partial line not joined: %q", tail)
	}

	sink.Write([]byte("trailing without newline"))
	sink.Close()
	tail, _, _ = bus.LogTail(buildID, 0)
	if !strings.HasSuffix(string(tail), "trailing without newline") {
		t.Errorf("Close did not drain the partial line: %q", tail)
	}

	build, _ := database.GetBuild(buildID)
	if build.Log != string(tail) {
		t.Errorf("DB log diverged from bus buffer:\n db=%q\nbus=%q", build.Log, tail)
	}
}

func TestSinkWriteAfterCloseDropped(t *testing.T) {
	database, bus, buildID := sinkFixtures(t)
	sink := newLogSink(buildID, "", database, bus)
	sink.Write([]byte("kept\n"))
	sink.Close()
	sink.Close() // idempotent
	sink.Write([]byte("dropped\n"))

	tail, _, _ := bus.LogTail(buildID, 0)
	if string(tail) != "kept\n" {
		t.Errorf("post-close write not dropped: %q", tail)
	}
}

func TestSinkManySmallWrites(t *testing.T) {
	database, bus, buildID := sinkFixtures(t)
	sink := newLogSink(buildID, "", database, bus)

	var want strings.Builder
	for i := 0; i < 500; i++ {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		sink.Write([]byte(line))
	}
	sink.Close()

	tail, _, _ := bus.LogTail(buildID, 0)
	if string(tail) != want.String() {
		t.Errorf("bus buffer mismatch: got %d bytes want %d", len(tail), want.Len())
	}
	build, _ := database.GetBuild(buildID)
	if build.Log != want.String() {
		t.Errorf("DB log mismatch: got %d bytes want %d", len(build.Log), want.Len())
	}
}
