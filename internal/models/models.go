package models

import "time"

type BuildStatus string

const (
	StatusPending BuildStatus = "pending"
	StatusRunning BuildStatus = "running"
	StatusSuccess BuildStatus = "success"
	StatusFailed  BuildStatus = "failed"
)

type Project struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	RepoURL            string    `json:"repo_url"`
	Branch             string    `json:"branch"`
	DockerfilePath     string    `json:"dockerfile_path"`
	ImageName          string    `json:"image_name"`
	DeployComposePath  string    `json:"deploy_compose_path,omitempty"`
	DeployServiceName  string    `json:"deploy_service_name,omitempty"`
	WebhookSecret      string    `json:"-"`
	CloneToken         string    `json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Build struct {
	ID            int64       `json:"id"`
	ProjectID     int64       `json:"project_id"`
	ProjectName   string      `json:"project_name,omitempty"`
	Status        BuildStatus `json:"status"`
	CommitSHA     string      `json:"commit_sha"`
	CommitMessage string      `json:"commit_message"`
	Log           string      `json:"log"`
	StartedAt     *time.Time  `json:"started_at"`
	FinishedAt    *time.Time  `json:"finished_at"`
	CreatedAt     time.Time   `json:"created_at"`
}
