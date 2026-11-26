// Command graft-observer serves the read model of the storage core to the
// git-ai view frontend.
//
// It is the server side of the view: the frontend holds no database
// connection, no worker host and no model key. It reads the storage core, and
// it answers with the picture of the fleet as JSON.
//
// Example:
//
//	graft-observer --database postgres://gitai@127.0.0.1:15432/gitai --listen 127.0.0.1:8790
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/observer"
)

const (
	// defaultListen is the address the service listens on when nothing else
	// is configured.
	defaultListen = "127.0.0.1:8790"
	// shutdownTimeout is how long the service waits for the requests that are
	// in flight before it stops.
	shutdownTimeout = 10 * time.Second
)

type options struct {
	listen       string
	database     string
	pollInterval time.Duration
	stateDir     string
	graftAddress string
	staticDir    string
	version      string

	// The credentials of the graft server: the observer asks it how far
	// every mirror is behind, and it performs the writes an operator asks
	// for.
	graftToken string
	graftBin   string
	graftUser  string
	storage    string

	// The operational files of the node: the queue of the mirrors, the
	// reports of the backup gate and the dumps of the storage core.
	queuePath  string
	reportsDir string
	dumpsDir   string

	// The CI server.
	ciDatabase string
	ciURL      string

	// The token a client carries to ask for a write.
	actionToken string

	mirrorTTL time.Duration
	ciTTL     time.Duration
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "graft-observer:", err)
		os.Exit(1)
	}
}

// parseOptions reads the command line and the environment. The environment
// wins over the defaults, the command line wins over the environment.
func parseOptions(args []string) (*options, error) {
	fs := flag.NewFlagSet("graft-observer", flag.ContinueOnError)
	var opts options
	fs.StringVar(&opts.listen, "listen", envOr("GRAFT_OBSERVER_LISTEN", defaultListen), "address to serve on, e.g. 127.0.0.1:8790")
	fs.StringVar(&opts.database, "database", os.Getenv("GRAFT_OBSERVER_DATABASE"), "URL of the storage core, e.g. postgres://gitai@127.0.0.1:15432/gitai")
	fs.DurationVar(&opts.pollInterval, "poll-interval", envDuration("GRAFT_OBSERVER_POLL_INTERVAL", observer.DefaultPollInterval), "how often the storage core is read")
	fs.StringVar(&opts.stateDir, "state-dir", os.Getenv("GRAFT_OBSERVER_STATE_DIR"), "directory of the journal of the events")
	fs.StringVar(&opts.graftAddress, "graft-address", os.Getenv("GRAFT_OBSERVER_ADDRESS"), "address of the graft server, shown as the origin of the writes")
	fs.StringVar(&opts.staticDir, "static-dir", os.Getenv("GRAFT_OBSERVER_STATIC"), "directory of the built frontend, when the service hosts it")
	fs.StringVar(&opts.version, "version", os.Getenv("GRAFT_OBSERVER_VERSION"), "version of the service")
	fs.StringVar(&opts.graftToken, "graft-token", envOr("GRAFT_OBSERVER_TOKEN", ""), "token of the graft server")
	fs.StringVar(&opts.graftBin, "graft-bin", envOr("GRAFT_OBSERVER_GRAFT_BIN", "graft"), "graft client used for the writes")
	fs.StringVar(&opts.graftUser, "graft-user", envOr("GRAFT_OBSERVER_GRAFT_USER", ""), "identity the writes are made with")
	fs.StringVar(&opts.storage, "storage", envOr("GRAFT_OBSERVER_STORAGE", "default"), "storage the writes address")
	fs.StringVar(&opts.queuePath, "queue-path", envOr("GRAFT_OBSERVER_QUEUE_PATH", ""), "file of the mirror queue of the node")
	fs.StringVar(&opts.reportsDir, "reports-dir", envOr("GRAFT_OBSERVER_REPORTS_DIR", ""), "directory of the reports of the backup gate")
	fs.StringVar(&opts.dumpsDir, "dumps-dir", envOr("GRAFT_OBSERVER_DUMPS_DIR", ""), "directory of the dumps of the storage core")
	fs.StringVar(&opts.ciDatabase, "ci-database", envOr("GRAFT_OBSERVER_CI_DATABASE", ""), "database of the CI server, read only")
	fs.StringVar(&opts.ciURL, "ci-url", envOr("GRAFT_OBSERVER_CI_URL", ""), "web base of the CI server")
	fs.StringVar(&opts.actionToken, "action-token", envOr("GRAFT_OBSERVER_ACTION_TOKEN", ""), "token a client carries to ask for a write")
	fs.DurationVar(&opts.mirrorTTL, "mirror-ttl", envDuration("GRAFT_OBSERVER_MIRROR_TTL", 10*time.Second), "how long a reading of the mirror telemetry is served")
	fs.DurationVar(&opts.ciTTL, "ci-ttl", envDuration("GRAFT_OBSERVER_CI_TTL", 8*time.Second), "how long a reading of the CI telemetry is served")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.database == "" {
		return nil, errors.New("--database is required: the observer reads the storage core")
	}
	return &opts, nil
}

// envOr reads a string from the environment, or the fallback.
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// envDuration reads a duration from the environment, or the fallback.
func envDuration(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		return parsed
	}
	if millis, err := strconv.Atoi(raw); err == nil {
		return time.Duration(millis) * time.Millisecond
	}
	return fallback
}

func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	// The token of the graft server may be given as a file, so that it
	// does not have to live in the environment of the service.
	graftToken := opts.graftToken
	if graftToken == "" {
		if path := os.Getenv("GRAFT_OBSERVER_TOKEN_FILE"); path != "" {
			if raw, err := os.ReadFile(path); err == nil {
				graftToken = strings.TrimSpace(string(raw))
			}
		}
	}

	service, err := observer.New(observer.Config{
		Database:     opts.database,
		Listen:       opts.listen,
		PollInterval: opts.pollInterval,
		StateDir:     opts.stateDir,
		GraftAddress: opts.graftAddress,
		StaticDir:    opts.staticDir,
		Version:      opts.version,

		GraftToken: graftToken,
		GraftBin:   opts.graftBin,
		GraftUser:  opts.graftUser,
		Storage:    opts.storage,

		QueuePath:  opts.queuePath,
		ReportsDir: opts.reportsDir,
		DumpsDir:   opts.dumpsDir,

		CIDatabase: opts.ciDatabase,
		CIURL:      opts.ciURL,

		ActionToken:    opts.actionToken,
		ActionsEnabled: graftToken != "" && opts.graftAddress != "",

		MirrorTTL: opts.mirrorTTL,
		CITTL:     opts.ciTTL,
	})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := service.Start(ctx); err != nil {
		return err
	}
	defer service.Stop()

	server := &http.Server{
		Addr:              opts.listen,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		fmt.Printf("graft-observer: serving %s\n", opts.listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stop()
	return server.Shutdown(shutdownCtx)
}
