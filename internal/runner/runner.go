package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

type Runner struct {
	DB     *db.DB
	Jobs   <-chan *models.Build
	done   chan struct{}
}

func New(database *db.DB, jobs <-chan *models.Build) *Runner {
	return &Runner{
		DB:   database,
		Jobs: jobs,
		done: make(chan struct{}),
	}
}

func (r *Runner) Start() {
	go r.loop()
}

func (r *Runner) Stop() {
	close(r.done)
}

func (r *Runner) loop() {
	for {
		select {
		case <-r.done:
			return
		case build := <-r.Jobs:
			r.runBuild(build)
		}
	}
}

func (r *Runner) runBuild(build *models.Build) {
	project, err := r.DB.GetProject(build.ProjectID)
	if err != nil {
		r.DB.UpdateBuildStatus(build.ID, models.StatusFailed, "Error loading project: "+err.Error())
		return
	}

	// Update status to running
	r.DB.UpdateBuildStatus(build.ID, models.StatusRunning, "Build started\n")

	var log strings.Builder

	logStep := func(msg string) {
		ts := time.Now().UTC().Format("15:04:05")
		line := fmt.Sprintf("[%s] %s\n", ts, msg)
		log.WriteString(line)
		r.DB.AppendBuildLog(build.ID, line)
	}

	logStep(fmt.Sprintf("Starting build for project: %s", project.Name))
	logStep(fmt.Sprintf("Commit: %s — %s", build.CommitSHA, build.CommitMessage))

	// Create temp workdir
	workDir, err := os.MkdirTemp("", fmt.Sprintf("builds-%d-", build.ID))
	if err != nil {
		r.failBuild(build.ID, log, "Failed to create temp dir: "+err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	logStep(fmt.Sprintf("Work dir: %s", workDir))

	// Step 1: Clone
	cloneURL := project.RepoURL
	if project.CloneToken != "" {
		// Inject token into HTTPS URL
		cloneURL = injectToken(cloneURL, project.CloneToken)
	}

	logStep(fmt.Sprintf("Cloning %s (branch: %s)", project.RepoURL, project.Branch))
	cloneCmd := newCmd("git", "clone", "--depth", "1", "--branch", project.Branch, cloneURL, workDir)
	cloneOutput, cloneErr := cloneCmd.CombinedOutput()
	log.WriteString(string(cloneOutput))
	r.DB.AppendBuildLog(build.ID, string(cloneOutput))

	if cloneErr != nil {
		r.failBuild(build.ID, log, fmt.Sprintf("Git clone failed: %v", cloneErr))
		return
	}

	// Checkout specific commit if provided and not "manual"
	if build.CommitSHA != "" && build.CommitSHA != "manual" {
		checkoutCmd := newCmd("git", "-C", workDir, "checkout", build.CommitSHA)
		coOut, coErr := checkoutCmd.CombinedOutput()
		log.WriteString(string(coOut))
		r.DB.AppendBuildLog(build.ID, string(coOut))
		if coErr != nil {
			logStep(fmt.Sprintf("Warning: checkout failed: %v (continuing with branch HEAD)", coErr))
		}
	}

	// Step 2: Docker build
	dockerfile := filepath.Join(workDir, project.DockerfilePath)
	imageTag := fmt.Sprintf("registry.fandoster.com/%s:latest", project.ImageName)

	logStep(fmt.Sprintf("Building Docker image: %s", imageTag))
	buildCmd := newCmd("docker", "build", "-t", imageTag, "-f", dockerfile, workDir)
	buildOutput, buildErr := buildCmd.CombinedOutput()
	log.WriteString(string(buildOutput))
	r.DB.AppendBuildLog(build.ID, string(buildOutput))

	if buildErr != nil {
		r.failBuild(build.ID, log, fmt.Sprintf("Docker build failed: %v", buildErr))
		return
	}

	// Step 3: Docker push
	logStep(fmt.Sprintf("Pushing image: %s", imageTag))
	pushCmd := newCmd("docker", "push", imageTag)
	pushOutput, pushErr := pushCmd.CombinedOutput()
	log.WriteString(string(pushOutput))
	r.DB.AppendBuildLog(build.ID, string(pushOutput))

	if pushErr != nil {
		r.failBuild(build.ID, log, fmt.Sprintf("Docker push failed: %v", pushErr))
		return
	}

	// Step 4: Deploy (if configured)
	if project.DeployComposePath != "" && project.DeployServiceName != "" {
		logStep(fmt.Sprintf("Deploying: docker compose -f %s up -d %s", project.DeployComposePath, project.DeployServiceName))
		deployCmd := newCmd("docker", "compose", "-f", project.DeployComposePath, "up", "-d", "--pull", "always", project.DeployServiceName)
		deployOutput, deployErr := deployCmd.CombinedOutput()
		log.WriteString(string(deployOutput))
		r.DB.AppendBuildLog(build.ID, string(deployOutput))

		if deployErr != nil {
			r.failBuild(build.ID, log, fmt.Sprintf("Deploy failed: %v", deployErr))
			return
		}
	} else {
		logStep("No deploy config — image pushed to registry. Watchtower will auto-deploy if watching.")
	}

	// Success!
	logStep("BUILD SUCCESS")
	r.DB.UpdateBuildStatus(build.ID, models.StatusSuccess, log.String())
}

func (r *Runner) failBuild(buildID int64, logBuilder strings.Builder, msg string) {
	logBuilder.WriteString(fmt.Sprintf("\n[ERROR] %s\n", msg))
	r.DB.UpdateBuildStatus(buildID, models.StatusFailed, logBuilder.String())
}

func injectToken(url, token string) string {
	// https://github.com/user/repo.git → https://token@github.com/user/repo.git
	return strings.Replace(url, "https://", "https://"+token+"@", 1)
}

func newCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no")
	return cmd
}
