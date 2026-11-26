package observer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	_ "github.com/mattn/go-sqlite3"    // registers the "sqlite3" driver
)

// Config is how the observer is told what to read and where to listen.
type Config struct {
	// Database is the URL of the storage core: "postgres://…" or
	// "sqlite:///path.db".
	Database string
	// Listen is the address the HTTP server listens on.
	Listen string
	// PollInterval is how often the service reads what changed.
	PollInterval time.Duration
	// StateDir is where the journal of the events is kept. Empty keeps it in
	// memory only.
	StateDir string
	// GraftAddress is the address of the graft server, shown to the clients as
	// the origin of the writes.
	GraftAddress string
	// ActionsEnabled tells the clients whether the service may write.
	ActionsEnabled bool
	// Version is the version of the service, shown by the health endpoint.
	Version string
	// StaticDir is the directory of the built frontend, when the service hosts it.
	StaticDir string
	// LabelDailyCalls is how many calls the galaxy namer may make a day.
	LabelDailyCalls int
}

// DefaultPollInterval is how often the service reads the storage core when
// nothing else is configured.
const DefaultPollInterval = 900 * time.Millisecond

// Observer serves the read model of the storage core.
type Observer struct {
	cfg       Config
	db        *sql.DB
	hub       *EventHub
	collector *collector
	startedAt time.Time
	stop      chan struct{}
}

// New returns an observer that reads the storage core of the configuration.
func New(cfg Config) (*Observer, error) {
	if cfg.Database == "" {
		return nil, errors.New("observer: the database of the storage core is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	db, err := openDatabase(cfg.Database)
	if err != nil {
		return nil, err
	}
	hub := NewEventHub(cfg.StateDir)
	return &Observer{
		cfg:       cfg,
		db:        db,
		hub:       hub,
		collector: newCollector(db, hub),
		startedAt: time.Now(),
		stop:      make(chan struct{}),
	}, nil
}

// openDatabase opens the storage core. The observer only reads, so it never
// applies the migrations of the storage core.
func openDatabase(dsn string) (*sql.DB, error) {
	driver := "pgx"
	if strings.HasPrefix(dsn, "sqlite:") {
		driver = "sqlite3"
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("observer: open the storage core: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

// Start reads the baseline of the fleet and keeps reading what changes.
func (o *Observer) Start(ctx context.Context) error {
	if err := o.collector.start(ctx); err != nil {
		return err
	}
	go o.pollLoop(ctx)
	return nil
}

// pollLoop reads the storage core every interval.
func (o *Observer) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.stop:
			return
		case <-ticker.C:
			pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = o.collector.poll(pollCtx)
			cancel()
		}
	}
}

// Stop ends the service.
func (o *Observer) Stop() error {
	close(o.stop)
	return o.db.Close()
}

// Poll reads what changed right now.
func (o *Observer) Poll(ctx context.Context) error {
	return o.collector.poll(ctx)
}

// Hub is the event hub of the service.
func (o *Observer) Hub() *EventHub {
	return o.hub
}

// sourceInfo says where the picture comes from.
func (o *Observer) sourceInfo() SourceInfo {
	o.collector.mu.RLock()
	warnings := append([]string(nil), o.collector.warnings...)
	lastError := o.collector.lastError
	startedAt := o.collector.startedAt
	o.collector.mu.RUnlock()
	if lastError != nil {
		warnings = append(warnings, fmt.Sprintf("poll error: %s", lastError))
	}
	if warnings == nil {
		warnings = []string{}
	}
	database := describeDatabase(o.cfg.Database)
	return SourceInfo{
		Mode:           "live",
		Label:          "storage core · " + database,
		Database:       &database,
		Address:        optional(o.cfg.GraftAddress),
		ActionsEnabled: o.cfg.ActionsEnabled,
		PollMs:         int(o.cfg.PollInterval / time.Millisecond),
		StartedAt:      isoTime(startedAt),
		Warnings:       warnings,
	}
}

// Snapshot is the picture of the fleet.
func (o *Observer) Snapshot() Snapshot {
	return o.collector.snapshot(o.sourceInfo())
}

// Health is the state of the service.
func (o *Observer) Health(ctx context.Context) HealthResponse {
	source := o.sourceInfo()
	warnings := append([]string(nil), source.Warnings...)
	ok := true
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := o.db.PingContext(probeCtx); err != nil {
		ok = false
		warnings = append(warnings, fmt.Sprintf("the storage core is unreachable: %s", err))
	}
	o.collector.mu.RLock()
	lastSuccess := o.collector.lastSuccessAt
	o.collector.mu.RUnlock()
	if !lastSuccess.IsZero() && time.Since(lastSuccess) > 5*o.cfg.PollInterval {
		warnings = append(warnings, "the storage core has not answered since "+isoTime(lastSuccess))
	}
	if warnings == nil {
		warnings = []string{}
	}
	return HealthResponse{
		OK:             ok,
		Mode:           "live",
		Database:       source.Database,
		ActionsEnabled: o.cfg.ActionsEnabled,
		UptimeSec:      int64(time.Since(o.startedAt).Seconds()),
		Version:        o.cfg.Version,
		Warnings:       warnings,
	}
}

// describeDatabase names a database without its credentials. The picture of the
// fleet is read by a browser, so the answer must never carry a password.
func describeDatabase(dsn string) string {
	if dsn == "" {
		return ""
	}
	if !strings.Contains(dsn, "://") {
		return dsn
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "database"
	}
	if parsed.Scheme == "sqlite" {
		return "sqlite:" + path.Base(parsed.Path)
	}
	host := parsed.Hostname()
	if host == "" {
		return "database"
	}
	if port := parsed.Port(); port != "" {
		host = host + ":" + port
	}
	name := strings.TrimPrefix(parsed.Path, "/")
	if name == "" {
		return host
	}
	return host + "/" + name
}

// optional returns a pointer to a value, or nil when the value is empty.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
