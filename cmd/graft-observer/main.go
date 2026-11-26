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
	service, err := observer.New(observer.Config{
		Database:     opts.database,
		Listen:       opts.listen,
		PollInterval: opts.pollInterval,
		StateDir:     opts.stateDir,
		GraftAddress: opts.graftAddress,
		StaticDir:    opts.staticDir,
		Version:      opts.version,
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
