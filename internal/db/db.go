package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/FanDoster/builds/internal/models"
)

type DB struct {
	conn *sql.DB
}

func Open(path string) (*DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	conn, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	conn.SetMaxOpenConns(1) // SQLite is single-writer
	conn.SetConnMaxLifetime(0)

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

func (d *DB) migrate() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			repo_url TEXT NOT NULL,
			branch TEXT NOT NULL DEFAULT 'main',
			dockerfile_path TEXT NOT NULL DEFAULT 'Dockerfile',
			image_name TEXT NOT NULL,
			deploy_compose_path TEXT DEFAULT '',
			deploy_service_name TEXT DEFAULT '',
			webhook_secret TEXT NOT NULL DEFAULT '',
			clone_token TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);

		CREATE TABLE IF NOT EXISTS builds (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'pending',
			commit_sha TEXT NOT NULL DEFAULT '',
			commit_message TEXT NOT NULL DEFAULT '',
			log TEXT NOT NULL DEFAULT '',
			started_at DATETIME,
			finished_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_builds_project ON builds(project_id, created_at DESC);
	`)
	return err
}

// --- Projects ---

func (d *DB) CreateProject(p *models.Project) error {
	p.CreatedAt = time.Now().UTC()
	p.UpdatedAt = p.CreatedAt
	res, err := d.conn.Exec(
		`INSERT INTO projects (name, repo_url, branch, dockerfile_path, image_name, deploy_compose_path, deploy_service_name, webhook_secret, clone_token, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.RepoURL, p.Branch, p.DockerfilePath, p.ImageName,
		p.DeployComposePath, p.DeployServiceName, p.WebhookSecret, p.CloneToken,
		p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	return nil
}

func (d *DB) GetProject(id int64) (*models.Project, error) {
	p := &models.Project{}
	err := d.conn.QueryRow(
		`SELECT id, name, repo_url, branch, dockerfile_path, image_name, deploy_compose_path, deploy_service_name, webhook_secret, clone_token, created_at, updated_at
		 FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.DockerfilePath, &p.ImageName,
		&p.DeployComposePath, &p.DeployServiceName, &p.WebhookSecret, &p.CloneToken,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (d *DB) GetProjectByName(name string) (*models.Project, error) {
	p := &models.Project{}
	err := d.conn.QueryRow(
		`SELECT id, name, repo_url, branch, dockerfile_path, image_name, deploy_compose_path, deploy_service_name, webhook_secret, clone_token, created_at, updated_at
		 FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.DockerfilePath, &p.ImageName,
		&p.DeployComposePath, &p.DeployServiceName, &p.WebhookSecret, &p.CloneToken,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (d *DB) ListProjects() ([]models.Project, error) {
	rows, err := d.conn.Query(
		`SELECT id, name, repo_url, branch, dockerfile_path, image_name, deploy_compose_path, deploy_service_name, created_at, updated_at
		 FROM projects ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []models.Project
	for rows.Next() {
		var p models.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.DockerfilePath, &p.ImageName,
			&p.DeployComposePath, &p.DeployServiceName, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (d *DB) UpdateProject(p *models.Project) error {
	p.UpdatedAt = time.Now().UTC()
	_, err := d.conn.Exec(
		`UPDATE projects SET name=?, repo_url=?, branch=?, dockerfile_path=?, image_name=?,
		 deploy_compose_path=?, deploy_service_name=?, webhook_secret=?, clone_token=?, updated_at=?
		 WHERE id=?`,
		p.Name, p.RepoURL, p.Branch, p.DockerfilePath, p.ImageName,
		p.DeployComposePath, p.DeployServiceName, p.WebhookSecret, p.CloneToken,
		p.UpdatedAt, p.ID,
	)
	return err
}

func (d *DB) DeleteProject(id int64) error {
	_, err := d.conn.Exec("DELETE FROM projects WHERE id = ?", id)
	return err
}

// --- Builds ---

func (d *DB) CreateBuild(b *models.Build) error {
	res, err := d.conn.Exec(
		`INSERT INTO builds (project_id, status, commit_sha, commit_message, created_at)
		 VALUES (?, ?, ?, ?, datetime('now'))`,
		b.ProjectID, b.Status, b.CommitSHA, b.CommitMessage,
	)
	if err != nil {
		return err
	}
	b.ID, _ = res.LastInsertId()
	return nil
}

func (d *DB) GetBuild(id int64) (*models.Build, error) {
	b := &models.Build{}
	err := d.conn.QueryRow(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.id = ?`, id,
	).Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
		&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (d *DB) ListBuildsByProject(projectID int64, limit int) ([]models.Build, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.project_id = ?
		 ORDER BY b.created_at DESC LIMIT ?`, projectID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func (d *DB) ListRecentBuilds(limit int) ([]models.Build, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 ORDER BY b.created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func (d *DB) ListBuildsByStatus(status models.BuildStatus) ([]models.Build, error) {
	rows, err := d.conn.Query(
		`SELECT b.id, b.project_id, p.name, b.status, b.commit_sha, b.commit_message, b.log, b.started_at, b.finished_at, b.created_at
		 FROM builds b JOIN projects p ON p.id = b.project_id
		 WHERE b.status = ?
		 ORDER BY b.created_at ASC`, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var builds []models.Build
	for rows.Next() {
		var b models.Build
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ProjectName, &b.Status, &b.CommitSHA, &b.CommitMessage,
			&b.Log, &b.StartedAt, &b.FinishedAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

func (d *DB) UpdateBuildStatus(id int64, status models.BuildStatus, log string) error {
	now := time.Now().UTC()
	var started, finished *time.Time

	switch status {
	case models.StatusRunning:
		started = &now
	case models.StatusSuccess, models.StatusFailed:
		finished = &now
	}

	_, err := d.conn.Exec(
		`UPDATE builds SET status=?, log=?, started_at=COALESCE(?, started_at), finished_at=? WHERE id=?`,
		status, log, started, finished, id,
	)
	return err
}

func (d *DB) AppendBuildLog(id int64, line string) error {
	_, err := d.conn.Exec(`UPDATE builds SET log = log || ? WHERE id = ?`, line, id)
	return err
}
