package runner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FanDoster/builds/internal/db"
	"github.com/FanDoster/builds/internal/models"
)

const DefaultBuildTimeout = 30 * time.Minute

type Runner struct {
	DB      *db.DB
	Jobs    <-chan *models.Build
	Timeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(database *db.DB, jobs <-chan *models.Build) *Runner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		DB:      database,
		Jobs:    jobs,
		Timeout: DefaultBuildTimeout,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (r *Runner) Start() {
	r.wg.Add(1)
	go r.loop()
}

// Stop cancels any in-flight build and waits for the worker to exit.
func (r *Runner) Stop() {
	r.cancel()
	r.wg.Wait()
}

func (r *Runner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
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

	ctx, cancel := context.WithTimeout(r.ctx, r.Timeout)
	defer cancel()

	// Update status to running
	r.DB.UpdateBuildStatus(build.ID, models.StatusRunning, "Build started\n")

	var log strings.Builder

	// All output goes through appendLog so secrets never reach the stored log.
	appendLog := func(text string) {
		text = scrubSecret(text, project.CloneToken)
		log.WriteString(text)
		r.DB.AppendBuildLog(build.ID, text)
	}
	logStep := func(msg string) {
		ts := time.Now().UTC().Format("15:04:05")
		appendLog(fmt.Sprintf("[%s] %s\n", ts, msg))
	}
	fail := func(msg string) {
		msg = scrubSecret(msg, project.CloneToken)
		log.WriteString(fmt.Sprintf("\n[ERROR] %s\n", msg))
		r.DB.UpdateBuildStatus(build.ID, models.StatusFailed, log.String())
	}

	logStep(fmt.Sprintf("Starting build for project: %s", project.Name))
	logStep(fmt.Sprintf("Commit: %s — %s", build.CommitSHA, build.CommitMessage))

	// Create temp workdir
	workDir, err := os.MkdirTemp("", fmt.Sprintf("builds-%d-", build.ID))
	if err != nil {
		fail("Failed to create temp dir: " + err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	logStep(fmt.Sprintf("Work dir: %s", workDir))

	// Validate Dockerfile path before doing any work.
	dockerfile, err := resolveDockerfile(workDir, project.DockerfilePath)
	if err != nil {
		fail(err.Error())
		return
	}

	// Step 1: Clone
	cloneURL := project.RepoURL
	if project.CloneToken != "" {
		cloneURL = injectToken(cloneURL, project.CloneToken)
	}

	logStep(fmt.Sprintf("Cloning %s (branch: %s)", project.RepoURL, project.Branch))
	cloneCmd := newCmd(ctx, "git", "clone", "--depth", "1", "--branch", project.Branch, cloneURL, workDir)
	cloneOutput, cloneErr := cloneCmd.CombinedOutput()
	appendLog(string(cloneOutput))

	if cloneErr != nil {
		fail(fmt.Sprintf("Git clone failed: %v%s", cloneErr, timeoutHint(ctx)))
		return
	}

	// Checkout specific commit if provided and not "manual"
	if build.CommitSHA != "" && build.CommitSHA != "manual" {
		checkoutCmd := newCmd(ctx, "git", "-C", workDir, "checkout", build.CommitSHA)
		coOut, coErr := checkoutCmd.CombinedOutput()
		appendLog(string(coOut))
		if coErr != nil {
			logStep(fmt.Sprintf("Warning: checkout failed: %v (continuing with branch HEAD)", coErr))
		}
	}

	// Step 2: Docker build
	imageTag := fmt.Sprintf("registry.fandoster.com/%s:latest", project.ImageName)

	logStep(fmt.Sprintf("Building Docker image: %s", imageTag))
	buildCmd := newCmd(ctx, "docker", "build", "-t", imageTag, "-f", dockerfile, workDir)
	buildOutput, buildErr := buildCmd.CombinedOutput()
	appendLog(string(buildOutput))

	if buildErr != nil {
		fail(fmt.Sprintf("Docker build failed: %v%s", buildErr, timeoutHint(ctx)))
		return
	}

	// Step 3: Docker push
	logStep(fmt.Sprintf("Pushing image: %s", imageTag))
	pushCmd := newCmd(ctx, "docker", "push", imageTag)
	pushOutput, pushErr := pushCmd.CombinedOutput()
	appendLog(string(pushOutput))

	if pushErr != nil {
		fail(fmt.Sprintf("Docker push failed: %v%s", pushErr, timeoutHint(ctx)))
		return
	}

	// Step 4: Deploy (if configured)
	if project.DeployComposePath != "" && project.DeployServiceName != "" {
		logStep(fmt.Sprintf("Deploying: docker compose -f %s up -d %s", project.DeployComposePath, project.DeployServiceName))
		deployCmd := newCmd(ctx, "docker", "compose", "-f", project.DeployComposePath, "up", "-d", "--pull", "always", project.DeployServiceName)
		deployOutput, deployErr := deployCmd.CombinedOutput()
		appendLog(string(deployOutput))

		if deployErr != nil {
			fail(fmt.Sprintf("Deploy failed: %v%s", deployErr, timeoutHint(ctx)))
			return
		}
	} else {
		logStep("No deploy config — image pushed to registry. Watchtower will auto-deploy if watching.")
	}

	// Success!
	logStep("BUILD SUCCESS")
	r.DB.UpdateBuildStatus(build.ID, models.StatusSuccess, log.String())
}

// timeoutHint annotates command failures caused by the build deadline or a
// server shutdown, which otherwise surface as an opaque "signal: killed".
func timeoutHint(ctx context.Context) string {
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return " (build timed out)"
	case context.Canceled:
		return " (build canceled by server shutdown)"
	}
	return ""
}

// resolveDockerfile joins the configured Dockerfile path with the work dir
// and rejects paths that escape the cloned repository.
func resolveDockerfile(workDir, path string) (string, error) {
	dockerfile := filepath.Join(workDir, path)
	rel, err := filepath.Rel(workDir, dockerfile)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dockerfile path %q escapes the repository checkout", path)
	}
	return dockerfile, nil
}

// injectToken adds a credential to an HTTP(S) clone URL, percent-encoding it
// so tokens containing special characters survive.
func injectToken(rawURL, token string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return rawURL
	}
	u.User = url.User(token)
	return u.String()
}

// scrubSecret masks a secret (and its percent-encoded form) in command output
// so it never lands in stored build logs.
func scrubSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	s = strings.ReplaceAll(s, secret, "***")
	if enc := url.User(secret).String(); enc != secret {
		s = strings.ReplaceAll(s, enc, "***")
	}
	return s
}

func newCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -o StrictHostKeyChecking=no")
	return cmd
}
