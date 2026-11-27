// Command graft-worker runs the jobs of a graft deployment on Temporal.
//
// The jobs of a deployment are the sweep of the mirror queue, the tiering of
// its fallbacks, the backup gate of the storage core, the dumps of its
// database, the gate of the local repositories and the backups of the
// checkouts. Every one of them is a workflow of a Temporal server: the
// schedule belongs to the server, the work belongs to the worker, and the
// history of a run is something a person can read.
//
// The worker holds the credentials of the node it works on, and the server
// holds the schedules.
//
// Example:
//
//	graft-worker worker --address 10.126.126.6:7233 --namespace graft
//	graft-worker register
//	graft-worker list
//	graft-worker run graft-mirror-sweep
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/jobs"
)

type options struct {
	address   string
	namespace string
	taskQueue string

	queueFile string
	queueLock string

	githubTokenFile string
	giteaTokenFile  string
	graftAddress    string
	graftToken      string
	graftStorage    string
	graftUser       string

	gateScript   string
	gateRegistry string
	gateReport   string
	gateSync     bool

	localScript       string
	localRoots        string
	localRegistry     string
	localReport       string
	localSync         bool
	localFullScript   string
	localFullCron     string
	localIncrScript   string
	localIncrCron     string
	localBackupScript string

	dumpScript   string
	dumpArgs     string
	dumpCron     string
	dumpDatabase string
	dumpPort     string
	dumpContain  string
	dumpDir      string
	dumpKeep     int

	dumpCIScript   string
	dumpCIArgs     string
	dumpCICron     string
	dumpCIDatabase string
	dumpCIPort     string
	dumpCIContain  string
	dumpCIDir      string
	dumpCIKeep     int

	importScript  string
	importRepo    string
	importRemote  string
	importEvery   time.Duration
	importTimeout time.Duration

	tokenFile string

	rest []string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "graft-worker:", err)
		os.Exit(1)
	}
}

// run dispatches the subcommands of the worker.
func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	// The credentials of the mirrors are read before any task runs.
	opts.loadCredentials()
	command := "worker"
	if len(opts.rest) > 0 {
		command = opts.rest[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch command {
	case "worker":
		return runWorker(ctx, opts)
	case "register":
		return runRegister(ctx, opts)
	case "unregister":
		return runUnregister(ctx, opts)
	case "list":
		return runList(ctx, opts)
	case "run":
		if len(opts.rest) < 2 {
			return errors.New("the run command names the job to run, e.g. graft-worker run graft-mirror-sweep")
		}
		return runJob(ctx, opts, opts.rest[1])
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// runWorker runs the worker until it is stopped.
func runWorker(ctx context.Context, o *options) error {
	c, err := jobs.NewClient(o.jobs())
	if err != nil {
		return err
	}
	defer c.Close()
	// The namespace of the jobs is created when it is missing, so that a
	// worker may be installed before anybody registered a schedule.
	if err := jobs.EnsureNamespace(ctx, c, o.namespace, "the jobs of a graft deployment"); err != nil {
		return fmt.Errorf("the namespace %s could not be prepared: %w", o.namespace, err)
	}
	w := jobs.NewWorker(c, o.jobs())
	fmt.Fprintf(os.Stdout, "graft-worker: the jobs of %s run on %s, the queue %s\n", o.address, o.namespace, o.taskQueue)
	if err := w.Start(); err != nil {
		return err
	}
	defer w.Stop()
	<-ctx.Done()
	fmt.Fprintln(os.Stdout, "graft-worker: stopped")
	return nil
}

// runRegister puts the schedules of the jobs of the node on the server.
func runRegister(ctx context.Context, o *options) error {
	c, err := jobs.NewClient(o.jobs())
	if err != nil {
		return err
	}
	defer c.Close()
	if err := jobs.EnsureNamespace(ctx, c, o.namespace, "the jobs of a graft deployment"); err != nil {
		return fmt.Errorf("the namespace %s could not be prepared: %w", o.namespace, err)
	}
	registered, err := jobs.Register(ctx, c, o.jobs())
	if err != nil {
		return err
	}
	for _, id := range registered {
		fmt.Fprintf(os.Stdout, "the job %s is on the schedule\n", id)
	}
	return nil
}

// runUnregister takes the schedules of the jobs of the node off the server.
func runUnregister(ctx context.Context, o *options) error {
	c, err := jobs.NewClient(o.jobs())
	if err != nil {
		return err
	}
	defer c.Close()
	removed, err := jobs.Unregister(ctx, c, o.jobs())
	if err != nil {
		return err
	}
	for _, id := range removed {
		fmt.Fprintf(os.Stdout, "the job %s is off the schedule\n", id)
	}
	return nil
}

// runList prints the schedules of the jobs of the node.
func runList(ctx context.Context, o *options) error {
	c, err := jobs.NewClient(o.jobs())
	if err != nil {
		return err
	}
	defer c.Close()
	statuses, err := jobs.List(ctx, c, o.jobs())
	if err != nil {
		return err
	}
	for _, s := range statuses {
		state := s.State
		if s.Paused {
			state = "paused"
		}
		next := "—"
		if !s.Next.IsZero() {
			next = s.Next.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(os.Stdout, "%-32s %-12s %-28s runs %d, next %s\n", s.ID, state, s.Workflow, s.Actions, next)
	}
	return nil
}

// runJob starts one job of the node once.
func runJob(ctx context.Context, o *options, id string) error {
	c, err := jobs.NewClient(o.jobs())
	if err != nil {
		return err
	}
	defer c.Close()
	runID, err := jobs.Run(ctx, c, o.jobs(), id)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "the job %s runs as %s\n", id, runID)
	return nil
}

// jobs returns the configuration of the node the worker works on.
func (o *options) jobs() jobs.Options {
	return jobs.Options{
		Address:   o.address,
		Namespace: o.namespace,
		TaskQueue: o.taskQueue,
		Mirror: jobs.MirrorOptions{
			QueueFile: o.queueFile,
			Address:   o.graftAddress,
			Token:     o.token(),
			Storage:   o.graftStorage,
			User:      o.graftUser,
		},
		Gate: jobs.GateOptions{
			Script:    o.gateScript,
			Registry:  o.gateRegistry,
			ReportDir: o.gateReport,
			Sync:      o.gateSync,
		},
		Local: jobs.LocalOptions{
			Script:            o.localScript,
			Roots:             splitList(o.localRoots),
			Registry:          o.localRegistry,
			ReportDir:         o.localReport,
			Sync:              o.localSync,
			FullScript:        o.localFullScript,
			FullCron:          o.localFullCron,
			IncrementalScript: o.localIncrScript,
			IncrementalCron:   o.localIncrCron,
		},
		Dump: jobs.DumpOptions{
			Script:    o.dumpScript,
			Args:      splitArgs(o.dumpArgs),
			Cron:      o.dumpCron,
			Database:  o.dumpDatabase,
			Port:      o.dumpPort,
			Container: o.dumpContain,
			Directory: o.dumpDir,
			Keep:      o.dumpKeep,
		},
		Import: jobs.ImportOptions{
			Script:     o.importScript,
			Repository: o.importRepo,
			Remote:     o.importRemote,
			Every:      o.importEvery,
			Timeout:    o.importTimeout,
		},
		DumpCI: jobs.DumpOptions{
			Script:    o.dumpCIScript,
			Args:      splitArgs(o.dumpCIArgs),
			Cron:      o.dumpCICron,
			Database:  o.dumpCIDatabase,
			Port:      o.dumpCIPort,
			Container: o.dumpCIContain,
			Directory: o.dumpCIDir,
			Keep:      o.dumpCIKeep,
		},
	}
}

// The files that hold the credentials of the mirrors. The queue names the
// variable a mirror takes, and the value of the variable is read here, when a
// task runs: a secret never sits in the queue and never rides on a command
// line.
const (
	defaultGitHubTokenFile = "$HOME/.config/laya-workflow/github.token"
	defaultGiteaTokenFile  = "$HOME/opt/git-ai/tokens/gitea"
)

// loadCredentials puts the credentials of the mirrors into the environment of
// the process, when the environment does not hold them already. A file that
// is not there is not an error: a deployment that mirrors to no such host
// does not need its credential.
func (o *options) loadCredentials() {
	loadCredential("GITHUB_TOKEN", o.githubTokenFile)
	loadCredential("GITEA_TOKEN", o.giteaTokenFile)
}

// loadCredential reads one credential file into the environment.
func loadCredential(name, path string) {
	if os.Getenv(name) != "" || path == "" {
		return
	}
	path = strings.ReplaceAll(path, "$HOME", os.Getenv("HOME"))
	value, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if token := strings.TrimSpace(string(value)); token != "" {
		os.Setenv(name, token)
	}
}

// token returns the credential of the operator. The credential is read from
// the file the operator named, because a value that sits in an environment
// file is a value that every process of the node can read.
func (o *options) token() string {
	if o.graftToken != "" {
		return o.graftToken
	}
	if o.tokenFile == "" {
		return ""
	}
	value, err := os.ReadFile(o.tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(value))
}

// parseOptions reads the command line and the environment. The environment
// wins over the defaults, the command line wins over the environment.
func parseOptions(args []string) (*options, error) {
	fs := flag.NewFlagSet("graft-worker", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.address, "address", envOr("GRAFT_WORKER_ADDRESS", jobs.DefaultAddress), "address of the Temporal server")
	fs.StringVar(&o.namespace, "namespace", envOr("GRAFT_WORKER_NAMESPACE", jobs.DefaultNamespace), "namespace the jobs live in")
	fs.StringVar(&o.taskQueue, "task-queue", envOr("GRAFT_WORKER_TASK_QUEUE", jobs.DefaultTaskQueue), "queue the worker polls")
	fs.StringVar(&o.queueFile, "queue-file", os.Getenv("GRAFT_WORKER_QUEUE_FILE"), "file the mirror queue is kept in")
	fs.StringVar(&o.githubTokenFile, "github-token-file", envOr("GRAFT_WORKER_GITHUB_TOKEN_FILE", defaultGitHubTokenFile), "file that holds the token of the GitHub mirror")
	fs.StringVar(&o.giteaTokenFile, "gitea-token-file", envOr("GRAFT_WORKER_GITEA_TOKEN_FILE", defaultGiteaTokenFile), "file that holds the token of the Gitea mirror")
	fs.StringVar(&o.graftAddress, "graft-address", os.Getenv("GRAFT_WORKER_GRAFT_ADDRESS"), "address of the graft server the queue publishes from")
	fs.StringVar(&o.graftToken, "graft-token", os.Getenv("GRAFT_WORKER_GRAFT_TOKEN"), "shared token of the operator")
	fs.StringVar(&o.tokenFile, "graft-token-file", os.Getenv("GRAFT_WORKER_GRAFT_TOKEN_FILE"), "file that holds the shared token of the operator")
	fs.StringVar(&o.graftStorage, "graft-storage", envOr("GRAFT_WORKER_GRAFT_STORAGE", "default"), "storage the repositories live in")
	fs.StringVar(&o.graftUser, "graft-user", os.Getenv("GRAFT_WORKER_GRAFT_USER"), "acting user of a write")
	fs.StringVar(&o.gateScript, "gate-script", os.Getenv("GRAFT_WORKER_GATE_SCRIPT"), "script of the backup gate of the storage core")
	fs.StringVar(&o.gateRegistry, "gate-registry", os.Getenv("GRAFT_WORKER_GATE_REGISTRY"), "file that names the origins of the repositories")
	fs.StringVar(&o.gateReport, "gate-report-dir", os.Getenv("GRAFT_WORKER_GATE_REPORT_DIR"), "directory the reports of the gate are written to")
	fs.BoolVar(&o.gateSync, "gate-sync", envBool("GRAFT_WORKER_GATE_SYNC", true), "publish what is missing before the gate verifies")
	fs.StringVar(&o.localScript, "local-script", os.Getenv("GRAFT_WORKER_LOCAL_SCRIPT"), "script of the gate of the local repositories")
	fs.StringVar(&o.localRoots, "local-roots", os.Getenv("GRAFT_WORKER_LOCAL_ROOTS"), "directories the local gate walks, comma separated")
	fs.StringVar(&o.localRegistry, "local-registry", os.Getenv("GRAFT_WORKER_LOCAL_REGISTRY"), "file that names the origins of the local repositories")
	fs.StringVar(&o.localReport, "local-report-dir", os.Getenv("GRAFT_WORKER_LOCAL_REPORT_DIR"), "directory the reports of the local gate are written to")
	fs.BoolVar(&o.localSync, "local-sync", envBool("GRAFT_WORKER_LOCAL_SYNC", true), "publish what is missing before the local gate verifies")
	fs.StringVar(&o.localFullScript, "local-full-script", os.Getenv("GRAFT_WORKER_LOCAL_FULL_SCRIPT"), "script that backs the checkouts up completely")
	fs.StringVar(&o.localFullCron, "local-full-cron", os.Getenv("GRAFT_WORKER_LOCAL_FULL_CRON"), "when the full backup runs, in the cron syntax")
	fs.StringVar(&o.localIncrScript, "local-incremental-script", os.Getenv("GRAFT_WORKER_LOCAL_INCREMENTAL_SCRIPT"), "script that backs the checkouts up since the last one")
	fs.StringVar(&o.localIncrCron, "local-incremental-cron", os.Getenv("GRAFT_WORKER_LOCAL_INCREMENTAL_CRON"), "when the incremental backup runs, in the cron syntax")
	fs.StringVar(&o.importScript, "import-script", os.Getenv("GRAFT_WORKER_IMPORT_SCRIPT"), "script that imports the state of a remote repository into the storage core")
	fs.StringVar(&o.importRepo, "import-repo", os.Getenv("GRAFT_WORKER_IMPORT_REPO"), "repository that is imported, e.g. quant_mt5.git")
	fs.StringVar(&o.importRemote, "import-remote", os.Getenv("GRAFT_WORKER_IMPORT_REMOTE"), "Git URL the state of the repository is imported from")
	fs.DurationVar(&o.importEvery, "import-every", envDuration("GRAFT_WORKER_IMPORT_EVERY", 30*time.Minute), "how often the import runs")
	fs.DurationVar(&o.importTimeout, "import-timeout", envDuration("GRAFT_WORKER_IMPORT_TIMEOUT", 30*time.Minute), "how long one import may run")
	fs.StringVar(&o.dumpScript, "dump-script", os.Getenv("GRAFT_WORKER_DUMP_SCRIPT"), "script that dumps the database of the node")
	fs.StringVar(&o.dumpArgs, "dump-args", os.Getenv("GRAFT_WORKER_DUMP_ARGS"), "arguments of the dump script, space separated")
	fs.StringVar(&o.dumpCron, "dump-cron", os.Getenv("GRAFT_WORKER_DUMP_CRON"), "when the dump of the storage core runs, in the cron syntax")
	fs.StringVar(&o.dumpCIScript, "dump-ci-script", os.Getenv("GRAFT_WORKER_DUMP_CI_SCRIPT"), "script that dumps the database of the CI server")
	fs.StringVar(&o.dumpCIArgs, "dump-ci-args", os.Getenv("GRAFT_WORKER_DUMP_CI_ARGS"), "arguments of the CI dump script, space separated")
	fs.StringVar(&o.dumpCICron, "dump-ci-cron", os.Getenv("GRAFT_WORKER_DUMP_CI_CRON"), "when the dump of the CI database runs, in the cron syntax")
	fs.StringVar(&o.dumpCIDatabase, "dump-ci-database", os.Getenv("GRAFT_WORKER_DUMP_CI_DATABASE"), "database of the CI server that is dumped")
	fs.StringVar(&o.dumpCIPort, "dump-ci-port", os.Getenv("GRAFT_WORKER_DUMP_CI_PORT"), "port PostgreSQL listens on inside its container")
	fs.StringVar(&o.dumpCIContain, "dump-ci-container", os.Getenv("GRAFT_WORKER_DUMP_CI_CONTAINER"), "container PostgreSQL runs in")
	fs.StringVar(&o.dumpCIDir, "dump-ci-dir", os.Getenv("GRAFT_WORKER_DUMP_CI_DIR"), "directory the dumps of the CI database are written to")
	fs.IntVar(&o.dumpCIKeep, "dump-ci-keep", envInt("GRAFT_WORKER_DUMP_CI_KEEP", 14), "how many dumps of the CI database are kept")
	fs.StringVar(&o.dumpDatabase, "dump-database", os.Getenv("GRAFT_WORKER_DUMP_DATABASE"), "database that is dumped")
	fs.StringVar(&o.dumpPort, "dump-port", os.Getenv("GRAFT_WORKER_DUMP_PORT"), "port PostgreSQL listens on inside its container")
	fs.StringVar(&o.dumpContain, "dump-container", os.Getenv("GRAFT_WORKER_DUMP_CONTAINER"), "container PostgreSQL runs in")
	fs.StringVar(&o.dumpDir, "dump-dir", os.Getenv("GRAFT_WORKER_DUMP_DIR"), "directory the dumps are written to")
	fs.IntVar(&o.dumpKeep, "dump-keep", envInt("GRAFT_WORKER_DUMP_KEEP", 14), "how many dumps of the database are kept")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	o.rest = fs.Args()
	return &o, nil
}

// envOr returns the value of the environment variable, or the fallback.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// envBool returns the value of the environment variable as a boolean, or the
// fallback when the variable is not set.
func envBool(name string, fallback bool) bool {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envInt returns the value of the environment variable as a number, or the
// fallback when the variable is not set.
func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// envDuration returns the value of the environment variable as a duration, or
// the fallback when the variable is not set.
func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// splitList splits a comma separated list, dropping the empty parts.
func splitList(raw string) []string {
	var list []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			list = append(list, part)
		}
	}
	return list
}

// splitArgs splits a line of arguments the way a shell would: on the
// spaces, and the quotes are kept out of the arguments.
func splitArgs(raw string) []string {
	var list []string
	for _, part := range strings.Fields(raw) {
		if part != "" {
			list = append(list, part)
		}
	}
	return list
}

// usage is the help of the worker.
const usage = `usage: graft-worker <command> [flags]

commands:
  worker         run the jobs of the node until the worker is stopped
  register       put the schedules of the jobs on the server
  unregister     take the schedules of the jobs off the server
  list           print the schedules of the jobs and when they run next
  run ID         start one job once, without waiting for its schedule

The configuration of the node is read from the command line and the
environment; see the flags of the command for the names.
`
