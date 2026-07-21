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
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	RepoURL           string    `json:"repo_url"`
	Branch            string    `json:"branch"`
	DockerfilePath    string    `json:"dockerfile_path"`
	ImageName         string    `json:"image_name"`
	DeployComposePath string    `json:"deploy_compose_path,omitempty"`
	DeployServiceName string    `json:"deploy_service_name,omitempty"`
	WebhookSecret     string    `json:"webhook_secret,omitempty"`
	CloneToken        string    `json:"clone_token,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Sanitize clears sensitive fields for API responses.
func (p *Project) Sanitize() {
	p.WebhookSecret = ""
	p.CloneToken = ""
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

// Duration returns a human-readable build duration, or "" if the build
// hasn't both started and finished. Value receiver so it is callable on
// range variables in templates.
func (b Build) Duration() string {
	if b.StartedAt == nil || b.FinishedAt == nil {
		return ""
	}
	return b.FinishedAt.Sub(*b.StartedAt).Round(time.Second).String()
}
