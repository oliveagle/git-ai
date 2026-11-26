package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The writes an operator asks for from the frontend.
//
// The client asks; the server does. The credentials of the graft server stay
// here: the browser holds nothing but a token of the observer, and the
// observer holds the token of the graft server.

// ActionLogLine is one line of the output of a write.
type ActionLogLine struct {
	At     string `json:"at"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// ActionResult is the answer of a write.
type ActionResult struct {
	OK      bool            `json:"ok"`
	Action  string          `json:"action"`
	Command *string         `json:"command"`
	Stdout  string          `json:"stdout"`
	Stderr  string          `json:"stderr"`
	Error   *string         `json:"error"`
	Events  []ActivityEvent `json:"events"`
	Log     []ActionLogLine `json:"log"`
}

// RepoCreateInput asks for a new repository.
type RepoCreateInput struct {
	Path          string   `json:"path"`
	DefaultBranch string   `json:"defaultBranch"`
	InitialCommit bool     `json:"initialCommit"`
	Branches      []string `json:"branches"`
}

// LandInput asks for a commit to be landed.
type LandInput struct {
	Workspace string `json:"workspace"`
	Branch    string `json:"branch"`
	Message   string `json:"message"`
	Resolve   bool   `json:"resolve"`
	Strategy  string `json:"strategy"`
}

// MirrorPushInput asks for a repository to be published to a mirror.
type MirrorPushInput struct {
	Repo     string   `json:"repo"`
	URL      string   `json:"url"`
	Branches []string `json:"branches"`
	Force    bool     `json:"force"`
}

// Actions performs the writes of the frontend.
type Actions struct {
	cfg Config
}

// NewActions returns the writer of the observer.
func NewActions(cfg Config) *Actions {
	return &Actions{cfg: cfg}
}

// Enabled tells whether the observer may write.
func (a *Actions) Enabled() bool {
	return a.cfg.GraftAddress != "" && a.cfg.GraftToken != "" && a.cfg.GraftBin != ""
}

// Authorized tells whether a client may ask for a write.
func (a *Actions) Authorized(token string) bool {
	if a.cfg.ActionToken == "" {
		return true
	}
	return strings.TrimSpace(token) == a.cfg.ActionToken
}

// CreateRepo registers a repository in the storage core.
func (a *Actions) CreateRepo(ctx context.Context, input RepoCreateInput) ActionResult {
	log := []ActionLogLine{}
	args := []string{"repo", "create", input.Path, "--default-branch", input.DefaultBranch}
	if input.InitialCommit {
		args = append(args, "--initial-commit")
	}
	for _, branch := range input.Branches {
		if strings.TrimSpace(branch) != "" {
			args = append(args, "--branch", strings.TrimSpace(branch))
		}
	}
	args = append(args, "--json")
	return a.run(ctx, "repo.create", args, "", log)
}

// Land commits the workspace and lands it on a branch.
func (a *Actions) Land(ctx context.Context, input LandInput) ActionResult {
	log := []ActionLogLine{}
	args := []string{"commit", "-m", input.Message, "--land", input.Branch}
	if input.Resolve {
		args = append(args, "--resolve")
	}
	if input.Strategy != "" && input.Strategy != "default" {
		args = append(args, "--strategy", input.Strategy)
	}
	return a.run(ctx, "land", args, input.Workspace, log)
}

// MirrorPush publishes a repository to a mirror and proves that the mirror
// carries the commits.
func (a *Actions) MirrorPush(ctx context.Context, input MirrorPushInput) ActionResult {
	log := []ActionLogLine{}
	args := []string{"mirror", "push", "--repo", input.Repo, "--url", input.URL}
	for _, branch := range input.Branches {
		if strings.TrimSpace(branch) != "" {
			args = append(args, "--branch", strings.TrimSpace(branch))
		}
	}
	if input.Force {
		args = append(args, "--force")
	}
	args = append(args, "--json")
	return a.run(ctx, "mirror.push", args, "", log)
}

// run performs one write and answers with what it did.
func (a *Actions) run(ctx context.Context, action string, args []string, workspace string, log []ActionLogLine) ActionResult {
	if !a.Enabled() {
		reason := "the writes are disabled: the address or the token of the graft server is not configured"
		return ActionResult{OK: false, Action: action, Error: stringPtr(reason), Events: []ActivityEvent{}, Log: log}
	}

	command := "graft " + strings.Join(args, " ")
	log = append(log, ActionLogLine{At: isoTime(time.Now()), Stream: "system", Text: "$ " + command})

	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.cfg.GraftBin, args...)
	cmd.Dir = workspace
	cmd.Env = a.environment()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	log = append(log, ActionLogLine{At: isoTime(time.Now()), Stream: "stdout", Text: a.redact(stdout.String())})
	log = append(log, ActionLogLine{At: isoTime(time.Now()), Stream: "stderr", Text: a.redact(stderr.String())})

	result := ActionResult{
		OK:      err == nil,
		Action:  action,
		Command: stringPtr(command),
		Stdout:  a.redact(stdout.String()),
		Stderr:  a.redact(stderr.String()),
		Events:  []ActivityEvent{},
		Log:     log,
	}
	if err != nil {
		result.Error = stringPtr(a.redact(err.Error()))
	}
	return result
}

// environment is the environment of the graft client.
func (a *Actions) environment() []string {
	env := os.Environ()
	env = append(env, "GRAFT_ADDRESS="+a.cfg.GraftAddress)
	env = append(env, "GRAFT_TOKEN="+a.cfg.GraftToken)
	if a.cfg.GraftUser != "" {
		env = append(env, "GRAFT_USER="+a.cfg.GraftUser)
	}
	if a.cfg.Storage != "" {
		env = append(env, "GRAFT_STORAGE="+a.cfg.Storage)
	}
	return env
}

// redact hides the secrets of a text before it is shown.
func redact(text string, secrets ...string) string {
	out := text
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, "[redacted]")
	}
	return out
}

// redact hides the secrets of the configuration of a text before it is shown.
func (a *Actions) redact(text string) string {
	return redact(text, a.cfg.GraftToken, a.cfg.ActionToken)
}

// decodeAction reads the body of a write.
func decodeAction(body []byte, into any) error {
	if len(body) == 0 {
		return fmt.Errorf("the body of the request is empty")
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("the body of the request is not understood: %w", err)
	}
	return nil
}
