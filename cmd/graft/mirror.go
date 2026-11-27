package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorsweep"
	"github.com/feder-cr/git-ai/sdk"
)

// mirrorSynopsis is the command line of graft mirror.
const mirrorSynopsis = "usage: graft mirror <push|status|queue|task> [flags]"

// mirrorUsage lists the subcommands of graft mirror.
const mirrorUsage = "mirror requires a subcommand: push (" + mirrorPushSynopsis + "), status (" + mirrorStatusSynopsis + "), queue (" + mirrorQueueSynopsis + "), task (" + mirrorTaskSynopsis + ")"

// mirrorPushSynopsis is the command line of graft mirror push.
const mirrorPushSynopsis = "graft mirror push --repo REPO --url URL --branch PATTERN [--branch PATTERN]... [--json]"

// mirrorHelp explains graft mirror below its synopsis.
const mirrorHelp = `Publishes selected branches of a Graft repository to another Git host as a
mirror. One command serves every host: there is no per-provider subcommand,
and the host is only the URL, so anything that speaks Git over HTTPS, SSH or
the scp-like form works the same way. The mirror is the compatibility path for
the workflows Graft does not replace yet, such as a pipeline that already runs
there.

The server makes the Git connection; the shared (operator) token authorizes
the RPC, so the remote credentials stay in the server process and never appear
in the command line or its output.

Credentials come from the environment, one variable per run:

  gitee.com   GITEE_TOKEN                    user "oauth2"
  github.com  GITHUB_TOKEN                   user "x-access-token"
  gitlab.com  GITLAB_TOKEN                   user "oauth2"

Those rows are a convenience, not a list of supported products: the host of
--url only picks a default variable and user, so the usual case needs no
credential flags at all. --token-env and --username name the credential for
any other host; the value of the variable is sent as HTTP Basic
authentication. --ssh-key (with --known-hosts) is the SSH alternative, and
a public mirror needs neither. An http:// URL with a token needs
--allow-insecure-http.

--branch is an allowlist and may be repeated; its patterns contain no
refs/heads/ prefix and may use *. Tags are always included, because they carry
the commits the branches name. Nothing under refs/graft/ is published: the
mirror is history, not Graft's review or verification state.

The push is a reconciliation, not an event: it compares the references again
on every run, so a scheduler can retry it after a network failure. By default
a remote branch whose history is not an ancestor of the Graft branch is kept
and reported, and the command exits 1. Inspect it first; --force says that the
Graft branch is the history to keep. Exit status: 0 when the selected
references are on the mirror, 1 when a divergent ref was kept, 2 when the
command line is wrong.`

// mirrorQueueHelp explains the mirror queue below the command line.
const mirrorQueueHelp = `The queue holds the mirrors of a client: the targets a Graft repository
is published to, and the tasks that published them. One queue serves every
host, so a GitHub mirror and a Gitee mirror are two targets of one queue.

  graft mirror queue add github-shop --repo shop.git \
    --url https://github.com/group/shop.git --branch main --interval 5m
  graft mirror queue list
  graft mirror queue run
  graft mirror queue worker

The queue is a file of the operator, by default the mirror-queue.json of the
user's configuration directory, and --queue-file names another one. It holds
no secret: a target names the environment variable that carries its token, and
the value is read when the task runs. A queue of a client that has no token in
its environment publishes to a public mirror.

--interval is how often a target is published; --interval 0 with --manual
keeps a target out of the schedule, so that it is only published when a
person asks for it. The worker publishes the targets that are due and waits
--tick between two looks. The queue run publishes the due targets once and
exits, which is the run of a cron job.

A run reads the changeset log of every repository the queue publishes, and
that log is the event the queue waits for: a repository that changed is
published at once, whatever its interval says, and the interval is the
fallback for what the log does not see. --idle-interval is the longest a
quiet target waits between two publications: the delay doubles once per
run that found the mirror up to date, up to the idle interval, and a change
puts the target back on its interval. A failing target stretches the same way
towards --failure-interval, which defaults to the idle interval, so that a
mirror nobody can reach stops consuming the schedule of the mirrors that
work. --task-timeout bounds one publication for the same reason.
--no-events reads no log, so that the schedule is the only trigger.

Every run records a task: what was published, when, and what happened. The
history is what an operator reads when a mirror stopped moving, and it is
kept in the same file.`

// mirrorFlags holds the flags of graft mirror push.
type mirrorFlags struct {
	repo              string
	url               string
	branches          stringList
	tokenEnv          string
	username          string
	sshKey            string
	knownHosts        string
	force             bool
	allowInsecureHTTP bool
	// target is the name of the registered mirror target a publication is
	// for: the server records what the mirror holds under that name.
	target string
}

// mirrorResult is the JSON output of graft mirror push.
type mirrorResult struct {
	// Repository is the Graft repository that was mirrored.
	Repository string `json:"repository"`
	// URL is the Git URL of the mirror.
	URL string `json:"url"`
	// Branches are the branch patterns the caller selected.
	Branches []string `json:"branches"`
	// Force tells whether divergent remote branches were overwritten.
	Force bool `json:"force,omitempty"`
	// DivergentRefs are the branches the server kept on the mirror.
	DivergentRefs []string `json:"divergent_refs,omitempty"`
}

// mirrorCommand returns the mirror command, the operator's push of a Graft
// repository to another Git server for the workflows that still run there.
func mirrorCommand() command {
	f := &mirrorFlags{}
	q := &mirrorQueueFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\n%s\n\n%s\n\n%s\n\nflags:\n", mirrorSynopsis, mirrorHelp, mirrorStatusHelp, mirrorQueueHelp)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.repo, "repo", "", "repository path, used with -address and -storage")
			fs.StringVar(&f.url, "url", "", "Git URL of the mirror: an HTTPS or SSH URL of any host, or a local path")
			fs.Var(&f.branches, "branch", "branch pattern to push (repeatable, * is a wildcard)")
			fs.StringVar(&f.tokenEnv, "token-env", "", "environment variable that holds the HTTPS token (default: the host's usual variable, such as GITHUB_TOKEN)")
			fs.StringVar(&f.username, "username", "", `HTTP Basic user for the token (default: by host, such as "oauth2")`)
			fs.StringVar(&f.sshKey, "ssh-key", "", "path of the private key for an SSH remote")
			fs.StringVar(&f.knownHosts, "known-hosts", "", "path of the host identities for strict SSH host-key checking")
			fs.BoolVar(&f.force, "force", false, "overwrite a divergent remote branch with the Graft branch")
			fs.BoolVar(&f.allowInsecureHTTP, "allow-insecure-http", false, "allow an http:// URL with a token")
			fs.StringVar(&f.target, "mirror-target", "", "name of the registered mirror target the publication is for (the server records what the mirror holds under it)")
			fs.DurationVar(&q.interval, "interval", -1, "how often a target of the queue is published, such as 5m (default 5m; 0: only when a person asks for it)")
			fs.BoolVar(&q.manual, "manual", false, "keep the target out of the schedule: publish it only when a person asks for it")
			fs.DurationVar(&q.idleInterval, "idle-interval", -1, "longest a quiet target waits between publications, such as 1h (default: the interval is fixed; requires --interval)")
			fs.DurationVar(&q.failureInterval, "failure-interval", -1, "longest a failing target waits between two attempts, such as 30m (default: the idle interval, or the interval)")
			fs.DurationVar(&q.tierActive, "active", time.Hour, "idle interval of mirror queue tier for a repository that changed within --active-within")
			fs.DurationVar(&q.tierRecent, "recent", 3*time.Hour, "idle interval of mirror queue tier for a repository that changed within --recent-within")
			fs.DurationVar(&q.tierDormant, "dormant", 12*time.Hour, "idle interval of mirror queue tier for a repository that changed longer ago")
			fs.DurationVar(&q.tierActiveWithin, "active-within", 24*time.Hour, "how recently a repository changed to be tiered active")
			fs.DurationVar(&q.tierRecentWithin, "recent-within", 7*24*time.Hour, "how recently a repository changed to be tiered recent")
			fs.DurationVar(&q.taskTimeout, "task-timeout", defaultTaskTimeout, "how long one publication may take before the run gives up on it (0: wait forever)")
			fs.DurationVar(&q.tick, "tick", defaultQueueTick, "how often the worker looks for the targets that are due")
			fs.IntVar(&q.concurrency, "concurrency", defaultQueueConcurrency, "how many tasks of the queue run at the same time")
			fs.StringVar(&q.queueFile, "queue-file", "", "file the mirror queue is kept in (env GRAFT_MIRROR_QUEUE)")
			fs.StringVar(&q.target, "target", "", "name of the one target of the queue a command works on")
			fs.IntVar(&q.limit, "limit", defaultTaskLimit, "how many tasks the task list prints")
			fs.BoolVar(&q.all, "all", false, "publish every enabled target, not only the ones that are due")
			fs.BoolVar(&q.noEvents, "no-events", false, "read no changeset log: publish only what the schedule says")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdMirror(ctx, out, o, f, q, args)
		},
	}
}

// mirrorCredential is what a known Git host calls its token and the HTTP Basic
// user that goes with it. It is a default, never a restriction: any URL works,
// and --token-env and --username override the entry.
type mirrorCredential struct {
	tokenEnv string
	username string
}

// mirrorCredentials are the hosts whose token convention is well known. A host
// that is not listed takes --token-env and --username as given, with "oauth2"
// as the user, which is what most installations accept.
var mirrorCredentials = map[string]mirrorCredential{
	"gitee.com":  {tokenEnv: "GITEE_TOKEN", username: "oauth2"},
	"github.com": {tokenEnv: "GITHUB_TOKEN", username: "x-access-token"},
	"gitlab.com": {tokenEnv: "GITLAB_TOKEN", username: "oauth2"},
}

// mirrorHost returns the host of a Git URL, empty when the URL has none. It
// understands https://host/path, ssh://git@host/path and the scp-like
// git@host:path form.
func mirrorHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	// scp-like: [user@]host:path
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		rest := raw[at+1:]
		if colon := strings.Index(rest, ":"); colon > 0 {
			return strings.ToLower(rest[:colon])
		}
	}
	return ""
}

// mirrorDefaults returns the token variable and the Basic user for a URL: the
// host convention, or the generic one when the host is not known.
// mirrorDefaults returns the token variable and the Basic user for a URL: the
// convention of its host, or the default of every other host.
func mirrorDefaults(raw string) mirrorCredential {
	c := mirrorsweep.Defaults(raw)
	return mirrorCredential{tokenEnv: c.TokenEnv, username: c.Username}
}

func cmdMirror(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, q *mirrorQueueFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", mirrorUsage)
	}
	switch args[0] {
	case "push":
		return cmdMirrorPush(ctx, out, o, f, args[1:])
	case "status":
		return cmdMirrorStatus(ctx, out, o, f, args[1:])
	case "queue":
		return cmdMirrorQueue(ctx, out, o, f, q, args[1:])
	case "task":
		return cmdMirrorTask(ctx, out, o, f, q, args[1:])
	default:
		return usagef("unknown mirror subcommand %q: %s", args[0], mirrorUsage)
	}
}

// cmdMirrorPush publishes the selected branches of one repository to one
// mirror. The command line is checked before the server is contacted.
func cmdMirrorPush(ctx context.Context, out io.Writer, o *options, f *mirrorFlags, args []string) error {
	if len(args) != 0 {
		return usagef("%s", mirrorPushSynopsis)
	}
	if f.repo == "" {
		return usagef("mirror push requires --repo REPO")
	}
	if f.url == "" {
		return usagef("mirror push requires --url URL")
	}
	if len(f.branches) == 0 {
		return usagef("mirror push requires --branch PATTERN: name the branches the mirror may publish")
	}
	if f.sshKey != "" && f.tokenEnv != "" {
		return usagef("mirror push takes either --token-env or --ssh-key, not both")
	}
	// A token over plain HTTP is sent in cleartext to every hop. The check is
	// here, before the server is contacted, so that a misconfigured schedule
	// fails with the usage status and sends nothing.
	if name, token := mirrorToken(mirrorAuth{usage: "mirror push", url: f.url, tokenEnv: f.tokenEnv, sshKey: f.sshKey}); token != "" && strings.HasPrefix(f.url, "http://") && !f.allowInsecureHTTP {
		return usagef("mirror push refuses to send %s over http://%s: use HTTPS, or pass --allow-insecure-http", name, strings.TrimPrefix(f.url, "http://"))
	}
	if _, err := mirrorPush(ctx, out, o, f); err != nil {
		return err
	}
	return nil
}

// mirrorPush asks the server to reconcile the selected references with the
// mirror.
func mirrorPush(ctx context.Context, out io.Writer, o *options, f *mirrorFlags) (*mirrorResult, error) {
	// The flags name files; the request carries the contents. The read happens
	// before the server is contacted, so a path that does not exist fails with
	// the usage status and publishes nothing.
	sshKey, err := mirrorCredentialFile("mirror push", "--ssh-key", f.sshKey)
	if err != nil {
		return nil, err
	}
	knownHosts, err := mirrorCredentialFile("mirror push", "--known-hosts", f.knownHosts)
	if err != nil {
		return nil, err
	}

	repo, _, _, err := gateRepo(ctx, o, &gateFlags{repo: f.repo})
	if err != nil {
		return nil, err
	}

	authorization, err := mirrorAuthorization(mirrorAuth{
		usage:    "mirror push",
		url:      f.url,
		tokenEnv: f.tokenEnv,
		username: f.username,
		sshKey:   f.sshKey,
	})
	if err != nil {
		return nil, err
	}

	res := &mirrorResult{
		Repository: f.repo,
		URL:        f.url,
		Branches:   append([]string(nil), f.branches...),
		Force:      f.force,
	}

	// A named target is what the server records the publication under. A
	// target the server does not know yet is registered with what the command
	// line says, so that a one-off publication opens the repository; a target
	// the server knows is left to the queue that declares it.
	if f.target != "" {
		if err := mirrorRegisterOneOffTarget(ctx, repo, f); err != nil {
			return nil, err
		}
	}

	answer, err := repo.Mirror(ctx, sdk.MirrorOptions{
		URL:                     f.url,
		Branches:                f.branches,
		HTTPAuthorizationHeader: authorization,
		SSHKey:                  sshKey,
		KnownHosts:              knownHosts,
		// The default is the safety net: a mirror that has unrelated history
		// is a person's decision, not a retry's.
		KeepDivergentRefs: !f.force,
		// A named target is what the server records the publication under,
		// and what the mirror policy decides the next push with.
		Target: f.target,
	})
	if err != nil {
		return nil, err
	}
	res.DivergentRefs = append(res.DivergentRefs, answer.DivergentRefs...)

	if o.jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintf(out, "mirrored %s to %s (branches: %s)\n", f.repo, mirrorDisplayURL(f.url), strings.Join(f.branches, ", "))
		for _, ref := range res.DivergentRefs {
			fmt.Fprintf(out, "kept the divergent ref %s\n", ref)
		}
		if len(res.DivergentRefs) > 0 {
			fmt.Fprintln(out, "the mirror is incomplete: inspect it, then repair it or run again with --force")
		}
	}

	if len(res.DivergentRefs) > 0 {
		return res, errors.New(strings.Join(res.DivergentRefs, ", "))
	}
	return res, nil
}

// mirrorAuth is the credential of one mirror: the URL it publishes to, the
// name of the environment variable that holds the token, the HTTP Basic user
// of the token, and the private key of an SSH remote. The push and the queue
// build one, so that both reach a host the same way.
type mirrorAuth struct {
	// usage names the command in a refusal, so that the reason says which
	// command refused.
	usage string
	// url is the Git URL of the mirror.
	url string
	// tokenEnv is the name of the environment variable that holds the token.
	tokenEnv string
	// username is the HTTP Basic user of the token.
	username string
	// sshKey is the path of the private key of an SSH remote.
	sshKey string
}

// mirrorAuthorization builds the Authorization header from the environment.
// The token itself is never printed, and never reaches argv.
// mirrorAuthorization builds the Authorization header from the environment.
// The token itself is never printed, and never reaches argv.
func mirrorAuthorization(a mirrorAuth) (string, error) {
	header, err := mirrorsweep.Authorization(mirrorsweep.Auth{
		Usage:    a.usage,
		URL:      a.url,
		TokenEnv: a.tokenEnv,
		Username: a.username,
		SSHKey:   a.sshKey,
	})
	if err != nil {
		return "", usagef("%s", err)
	}
	return header, nil
}

// mirrorToken returns the token variable of the run and its value: the one the
// caller named, else the one the host of the URL implies, and empty when the
// variable is unset.
func mirrorToken(a mirrorAuth) (string, string) {
	return mirrorsweep.Token(mirrorsweep.Auth{
		Usage:    a.usage,
		URL:      a.url,
		TokenEnv: a.tokenEnv,
		Username: a.username,
		SSHKey:   a.sshKey,
	})
}

// mirrorCredentialFile reads a credential file that the caller named by path,
// such as --ssh-key or --known-hosts. The request of the server carries the
// contents, because the server writes them into a private file of its own and
// lets the Git connection use it for the length of the call. The read happens
// in the client so that the path is resolved where the person wrote it, and a
// path that is wrong fails before any Git connection is made.
func mirrorCredentialFile(usage, flag, path string) (string, error) {
	content, err := mirrorsweep.CredentialFile(usage, flag, path)
	if err != nil {
		return "", usagef("%s", err)
	}
	return content, nil
}

// mirrorDisplayURL hides the user information of a URL that a person may have
// put a credential in: the mirror is printed in logs and chat.
func mirrorDisplayURL(raw string) string {
	return mirrorsweep.DisplayURL(raw)
}

func mirrorRegisterOneOffTarget(ctx context.Context, repo *sdk.Repo, f *mirrorFlags) error {
	targets, _, _, err := repo.ListMirrorTargets(ctx, "")
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target.Name == f.target {
			return nil
		}
	}

	_, err = repo.RegisterMirrorTarget(ctx, sdk.MirrorTargetOptions{
		Name:     f.target,
		URL:      f.url,
		Branches: f.branches,
		Enabled:  true,
	})
	if err != nil {
		return fmt.Errorf("registering the mirror target %q with the server: %w", f.target, err)
	}
	return nil
}
