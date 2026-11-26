package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/feder-cr/git-ai/sdk"
)

// The mirror and the backup telemetry of the fleet.
//
// Three authorities know about a mirror target: the operator's queue, which
// holds the plan (the interval, the next run and the last tasks), the graft
// server, which owns the measured state (how far the mirror is behind, when
// it published last), and the backup gate, which reads the origins back and
// says whether a backup counts.
//
// The observer joins them here, on the server. A client reads one document
// instead of running a hundred probes over ssh, and the pages that need this
// answer as fast as the rest of the fleet.

// MirrorRun is one observed publication, as the mirror worker recorded it.
type MirrorRun struct {
	ID         string  `json:"id"`
	Target     string  `json:"target"`
	Repository string  `json:"repository"`
	Status     string  `json:"status"`
	QueuedAt   *string `json:"queuedAt"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	DurationMs *int64  `json:"durationMs"`
	Error      *string `json:"error"`
}

// MirrorTargetIntel is one mirror target, joined across the three places
// that know about it.
type MirrorTargetIntel struct {
	Name          string            `json:"name"`
	Repository    string            `json:"repository"`
	URL           string            `json:"url"`
	Host          string            `json:"host"`
	Branches      []string          `json:"branches"`
	Enabled       bool              `json:"enabled"`
	IntervalMs    int64             `json:"intervalMs"`
	NextRunAt     *string           `json:"nextRunAt"`
	OverdueMs     int64             `json:"overdueMs"`
	OnSchedule    bool              `json:"onSchedule"`
	CadenceMs     *int64            `json:"cadenceMs"`
	ExpectedRuns  int64             `json:"expectedRuns"`
	ObservedRuns  int64             `json:"observedRuns"`
	LastStatus    string            `json:"lastStatus"`
	LastRunAt     *string           `json:"lastRunAt"`
	LastOkAt      *string           `json:"lastOkAt"`
	LastOkAgeMs   *int64            `json:"lastOkAgeMs"`
	FailStreak    int64             `json:"failStreak"`
	SuccessRate   *float64          `json:"successRate"`
	AvgDurationMs *int64            `json:"avgDurationMs"`
	Lag           *int64            `json:"lag"`
	SyncStatus    *string           `json:"syncStatus"`
	PublishedAt   *string           `json:"publishedAt"`
	Published     map[string]string `json:"published"`
	Error         *string           `json:"error"`
	Runs          []MirrorRun       `json:"runs"`
}

// MirrorRepoIntel is one repository of the mirror fabric.
type MirrorRepoIntel struct {
	Repository string   `json:"repository"`
	Branches   []string `json:"branches"`
	Targets    int      `json:"targets"`
	Healthy    int      `json:"healthy"`
	Failed     int      `json:"failed"`
	Late       int      `json:"late"`
	WorstLag   *int64   `json:"worstLag"`
	LastOkAt   *string  `json:"lastOkAt"`
}

// MirrorSummary counts the mirror fabric.
type MirrorSummary struct {
	Targets       int      `json:"targets"`
	Enabled       int      `json:"enabled"`
	Disabled      int      `json:"disabled"`
	OK            int      `json:"ok"`
	Failed        int      `json:"failed"`
	Idle          int      `json:"idle"`
	Overdue       int      `json:"overdue"`
	Stale         int      `json:"stale"`
	AvgLag        *float64 `json:"avgLag"`
	MaxLag        *int64   `json:"maxLag"`
	SuccessRate   *float64 `json:"successRate"`
	ObservedRuns  int64    `json:"observedRuns"`
	ExpectedRuns  int64    `json:"expectedRuns"`
	OnSchedulePct *int64   `json:"onSchedulePct"`
	HistoryFrom   *string  `json:"historyFrom"`
	HistoryTo     *string  `json:"historyTo"`
	LastTaskAt    *string  `json:"lastTaskAt"`
	LastOkAt      *string  `json:"lastOkAt"`
}

// HostDisk is the disk of one host that matters to the fleet.
type HostDisk struct {
	Host       string `json:"host"`
	Role       string `json:"role"`
	Mount      string `json:"mount"`
	TotalBytes int64  `json:"totalBytes"`
	FreeBytes  int64  `json:"freeBytes"`
	UsedPct    int    `json:"usedPct"`
}

// BackupOriginStatus is the verdict of the backup gate about one origin.
type BackupOriginStatus string

const (
	BackupInSync  BackupOriginStatus = "in_sync"
	BackupDrifted BackupOriginStatus = "drifted"
	BackupMissing BackupOriginStatus = "missing"
	BackupFailed  BackupOriginStatus = "failed"
)

// BackupOrigin is one (repository, backup origin) check of the gate.
type BackupOrigin struct {
	Repository string             `json:"repository"`
	Origin     string             `json:"origin"`
	URL        string             `json:"url"`
	Status     BackupOriginStatus `json:"status"`
	Detail     *string            `json:"detail"`
}

// BackupDump is the newest dump of the storage core.
type BackupDump struct {
	Path      string  `json:"path"`
	At        *string `json:"at"`
	SizeBytes int64   `json:"sizeBytes"`
}

// BackupGateSummary is the verdict of the backup gate.
type BackupGateSummary struct {
	CheckedAt    *string        `json:"checkedAt"`
	Result       string         `json:"result"`
	Repositories int            `json:"repositories"`
	InSync       int            `json:"inSync"`
	Drifted      int            `json:"drifted"`
	Missing      int            `json:"missing"`
	Failed       int            `json:"failed"`
	Origins      []BackupOrigin `json:"origins"`
	Dump         *BackupDump    `json:"dump"`
}

// BackupReportResponse is what the backups page reads.
type BackupReportResponse struct {
	OK          bool               `json:"ok"`
	GeneratedAt string             `json:"generatedAt"`
	Source      BackupSource       `json:"source"`
	Gate        *BackupGateSummary `json:"gate"`
	Repos       []MirrorRepoIntel  `json:"repos"`
	Hosts       []HostDisk         `json:"hosts"`
}

// BackupSource says where the backup report comes from.
type BackupSource struct {
	Host      string  `json:"host"`
	CheckedAt *string `json:"checkedAt"`
	Error     *string `json:"error"`
}

// MirrorIntelResponse is what the mirrors page reads.
type MirrorIntelResponse struct {
	OK          bool                `json:"ok"`
	GeneratedAt string              `json:"generatedAt"`
	CachedForMs int64               `json:"cachedForMs"`
	Source      MirrorSource        `json:"source"`
	Policy      *MirrorPolicy       `json:"policy"`
	Summary     MirrorSummary       `json:"summary"`
	Repos       []MirrorRepoIntel   `json:"repos"`
	Targets     []MirrorTargetIntel `json:"targets"`
	Runs        []MirrorRun         `json:"runs"`
	Backup      *BackupGateSummary  `json:"backup"`
	Hosts       []HostDisk          `json:"hosts"`
}

// MirrorSource says where the mirror telemetry comes from.
type MirrorSource struct {
	Host       string   `json:"host"`
	QueuePath  string   `json:"queuePath"`
	QueueMtime *string  `json:"queueMtime"`
	LagProbes  int      `json:"lagProbes"`
	Error      *string  `json:"error"`
	Warnings   []string `json:"warnings"`
}

// MirrorPolicy is the mirror policy the server enforces.
type MirrorPolicy struct {
	Required bool  `json:"required"`
	WarnLag  int64 `json:"warnLag"`
	DenyLag  int64 `json:"denyLag"`
}

const (
	// defaultIntervalMs is the interval a target is assumed to keep when the
	// queue does not say one.
	defaultIntervalMs = 5 * 60 * 1000
	// mirrorHistoryWindowMs is how far back the runs are looked at.
	mirrorHistoryWindowMs = 60 * 60 * 1000
	// mirrorRecentRuns is how many runs are handed to a client.
	mirrorRecentRuns = 80
	// mirrorRunsPerTarget is how many runs of one target are kept.
	mirrorRunsPerTarget = 24
	// mirrorProbeJobs is how many repositories are asked at once.
	mirrorProbeJobs = 8
)

// DatabaseProbe says whether the storage core answers, and how fast.
type DatabaseProbe struct {
	Reachable bool    `json:"reachable"`
	LatencyMs *int64  `json:"latencyMs"`
	SizeBytes *int64  `json:"sizeBytes"`
	Error     *string `json:"error"`
	CheckedAt string  `json:"checkedAt"`
}

// PollProbe says how old the last reading of the storage core is.
type PollProbe struct {
	LastPollAt *string `json:"lastPollAt"`
	AgeMs      *int64  `json:"ageMs"`
}

// MirrorSourceProbe says whether the mirror telemetry could be read.
type MirrorSourceProbe struct {
	OK        bool    `json:"ok"`
	Error     *string `json:"error"`
	Host      *string `json:"host"`
	LagProbes int     `json:"lagProbes"`
}

// SystemProbe is what the health page reads under the verdict.
type SystemProbe struct {
	Database     DatabaseProbe      `json:"database"`
	Poll         PollProbe          `json:"poll"`
	MirrorSource *MirrorSourceProbe `json:"mirrorSource"`
	Mirrors      *MirrorSummary     `json:"mirrors"`
	Backup       *BackupGateSummary `json:"backup"`
	Hosts        []HostDisk         `json:"hosts"`
	Nodes        []NodeSummary      `json:"nodes"`
}

// MirrorIntel reads the mirror and backup telemetry of this host.
type MirrorIntel struct {
	cfg Config

	mu       sync.Mutex
	cached   *mirrorIntelValue
	inflight chan struct{}
}

// mirrorIntelValue is one collected reading.
type mirrorIntelValue struct {
	at        time.Time
	response  MirrorIntelResponse
	backup    BackupReportResponse
	collected bool
}

// NewMirrorIntel returns the collector of the mirror and backup telemetry.
func NewMirrorIntel(cfg Config) *MirrorIntel {
	return &MirrorIntel{cfg: cfg}
}

// Get answers with the telemetry. A reading that is younger than the
// configured age is served as it is; an older one is served stale while it
// is collected again in the background, so a slow worker host never holds
// the page.
func (m *MirrorIntel) Get(ctx context.Context) (MirrorIntelResponse, BackupReportResponse) {
	now := time.Now()
	m.mu.Lock()
	cached := m.cached
	m.mu.Unlock()

	if cached != nil && now.Sub(cached.at) < m.cfg.MirrorTTL {
		response := cached.response
		response.CachedForMs = now.Sub(cached.at).Milliseconds()
		return response, cached.backup
	}

	if cached != nil {
		// A stale reading is still a reading: it is handed over now and the
		// worker host is asked again in the background.
		go m.refresh(context.WithoutCancel(ctx))
		response := cached.response
		response.CachedForMs = now.Sub(cached.at).Milliseconds()
		return response, cached.backup
	}

	// Nothing has been collected yet: the first reading has to be waited
	// for.
	select {
	case <-m.refresh(ctx):
	case <-time.After(60 * time.Second):
	}
	m.mu.Lock()
	value := m.cached
	m.mu.Unlock()
	if value == nil {
		reason := "the mirror telemetry could not be collected"
		return MirrorIntelResponse{
				OK:          false,
				GeneratedAt: isoTime(now),
				Summary:     MirrorSummary{},
				Repos:       []MirrorRepoIntel{},
				Targets:     []MirrorTargetIntel{},
				Runs:        []MirrorRun{},
				Source:      MirrorSource{Error: &reason, QueuePath: m.cfg.QueuePath},
			}, BackupReportResponse{
				OK:          false,
				GeneratedAt: isoTime(now),
				Gate:        nil,
				Repos:       []MirrorRepoIntel{},
				Hosts:       []HostDisk{},
				Source:      BackupSource{Error: &reason},
			}
	}
	return value.response, value.backup
}

// refresh collects the telemetry once. It returns the channel that is
// closed when the collection is done, so that a caller that needs the first
// reading waits for it and the others do not collect twice.
func (m *MirrorIntel) refresh(ctx context.Context) <-chan struct{} {
	m.mu.Lock()
	if m.inflight != nil {
		done := m.inflight
		m.mu.Unlock()
		return done
	}
	done := make(chan struct{})
	m.inflight = done
	m.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			m.mu.Lock()
			m.inflight = nil
			m.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()

		response, backup := m.collect(ctx)
		m.mu.Lock()
		m.cached = &mirrorIntelValue{at: time.Now(), response: response, backup: backup, collected: true}
		m.mu.Unlock()
	}()
	return done
}

// collect reads the queue, the measured state of the mirrors, the backup gate
// and the disk, and joins them.
func (m *MirrorIntel) collect(ctx context.Context) (MirrorIntelResponse, BackupReportResponse) {
	now := time.Now()
	queue, queueErr := readQueueFile(m.cfg.QueuePath)
	var mtime *string
	if info, err := os.Stat(m.cfg.QueuePath); err == nil {
		stamp := isoTime(info.ModTime())
		mtime = &stamp
	}

	targets := queue.Targets
	tasks := queue.Tasks

	repositories := map[string]bool{}
	for _, target := range targets {
		if target.Repository != "" {
			repositories[target.Repository] = true
		}
	}

	lag, policy, probeErr := m.measure(ctx, sortedKeys(repositories))
	if probeErr != nil && len(lag) == 0 {
		lag = map[string]mirrorLag{}
	}

	gate, dump := readGateReport(m.cfg.ReportsDir), readNewestDump(m.cfg.DumpsDir)
	if gate != nil && dump != nil {
		gate.Dump = dump
	}

	disk := localDisk()

	// A repository the queue names but the storage core does not hold is a
	// warning, not a failure: the other mirrors are still measured.
	var reason *string
	var warnings []string
	switch {
	case queueErr != nil:
		reason = stringPtr("the mirror queue could not be read: " + queueErr.Error())
	case len(targets) == 0:
		reason = stringPtr("the mirror queue is empty or unreadable on this host")
	case probeErr != nil && len(lag) == 0:
		reason = stringPtr(probeErr.Error())
	case probeErr != nil:
		warnings = append(warnings, probeErr.Error())
	}
	if warnings == nil {
		warnings = []string{}
	}

	intel := composeMirrors(m.cfg, targets, tasks, lag, policy, gate, disk, mtime, reason, warnings, now)

	repos := intel.Repos
	backup := BackupReportResponse{
		OK:          reason == nil,
		GeneratedAt: isoTime(now),
		Gate:        gate,
		Repos:       repos,
		Hosts:       intel.Hosts,
		Source: BackupSource{
			Host:      diskHost(),
			CheckedAt: gateTime(gate),
			Error:     reason,
		},
	}
	return intel, backup
}

// measure asks the graft server how far every mirror is behind. When the
// server cannot be reached, the last publication the storage core recorded is
// used instead: it is older, but it is true.
func (m *MirrorIntel) measure(ctx context.Context, repositories []string) (map[string]mirrorLag, *MirrorPolicy, error) {
	if len(repositories) == 0 {
		return map[string]mirrorLag{}, nil, nil
	}

	client, err := m.client()
	if err != nil {
		return m.measuredFromStorageCore(ctx, repositories)
	}
	defer client.Close()

	lag := map[string]mirrorLag{}
	var policy *MirrorPolicy
	var firstErr error
	var mu sync.Mutex

	workers := mirrorProbeJobs
	if workers > len(repositories) {
		workers = len(repositories)
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repository := range jobs {
				callCtx, callCancel := context.WithTimeout(ctx, 15*time.Second)
				targets, targetPolicy, _, err := client.Repo(repository).ListMirrorTargets(callCtx, "")
				callCancel()
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("the graft server did not answer for %q: %w", repository, err)
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				if policy == nil {
					policy = &MirrorPolicy{
						Required: targetPolicy.Required,
						WarnLag:  int64(targetPolicy.WarnLag),
						DenyLag:  int64(targetPolicy.DenyLag),
					}
				}
				for _, target := range targets {
					entry := mirrorLag{
						Status:    target.Status,
						Published: target.Published,
					}
					if entry.Published == nil {
						entry.Published = map[string]string{}
					}
					lagValue := int64(target.Lag)
					entry.Lag = &lagValue
					if !target.PublishedAt.IsZero() {
						stamp := isoTime(target.PublishedAt)
						entry.PublishedAt = &stamp
					}
					if target.Error != "" {
						entry.Error = stringPtr(target.Error)
					}
					lag[target.Name] = entry
				}
				mu.Unlock()
			}
		}()
	}
	for _, repository := range repositories {
		jobs <- repository
	}
	close(jobs)
	wg.Wait()

	if firstErr != nil && len(lag) == 0 {
		fallback, policyFallback, _ := m.measuredFromStorageCore(ctx, repositories)
		if len(fallback) > 0 {
			return fallback, policyFallback, nil
		}
		return lag, policy, firstErr
	}
	return lag, policy, firstErr
}

// measuredFromStorageCore reads the last publication of every mirror target
// from the storage core. It knows what the mirror published and when, but not
// how far behind it is.
func (m *MirrorIntel) measuredFromStorageCore(ctx context.Context, repositories []string) (map[string]mirrorLag, *MirrorPolicy, error) {
	lag := map[string]mirrorLag{}
	if m.cfg.Database == "" {
		return lag, nil, fmt.Errorf("the storage core is not configured")
	}
	db, err := openDatabase(m.cfg.Database)
	if err != nil {
		return lag, nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT t.relative_path, t.name, t.url, t.enabled, s.status, s.published_at, s.error, s.branches
		FROM graft_mirror_targets t
		LEFT JOIN graft_mirror_syncs s
		  ON s.storage = t.storage AND s.relative_path = t.relative_path AND s.name = t.name
		ORDER BY t.relative_path, t.name`)
	if err != nil {
		return lag, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			repository, name, url, status, publishedAt, errorText, branches string
			enabled                                                         bool
		)
		if err := rows.Scan(&repository, &name, &url, &enabled, &status, &publishedAt, &errorText, &branches); err != nil {
			continue
		}
		if !enabled {
			continue
		}
		entry := mirrorLag{Status: status, Published: map[string]string{}}
		if publishedAt != "" {
			if parsed, err := time.Parse(time.RFC3339, publishedAt); err == nil {
				stamp := isoTime(parsed)
				entry.PublishedAt = &stamp
			}
		}
		if errorText != "" {
			entry.Error = stringPtr(errorText)
		}
		if branches != "" {
			published := map[string]string{}
			if err := json.Unmarshal([]byte(branches), &published); err == nil {
				entry.Published = published
			}
		}
		lag[name] = entry
	}
	return lag, nil, rows.Err()
}

// client returns a connection to the graft server.
func (m *MirrorIntel) client() (*sdk.Client, error) {
	if m.cfg.GraftAddress == "" {
		return nil, fmt.Errorf("the address of the graft server is not configured")
	}
	if m.cfg.GraftToken == "" {
		return nil, fmt.Errorf("the token of the graft server is not configured")
	}
	return sdk.Dial(context.Background(), m.cfg.GraftAddress, sdk.WithToken(m.cfg.GraftToken))
}

// queueFile is the operator's queue of the mirror targets.
type queueFile struct {
	Targets []queueTarget `json:"targets"`
	Tasks   []queueTask   `json:"tasks"`
}

// queueTarget is one planned publication.
type queueTarget struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	URL        string   `json:"url"`
	Branches   []string `json:"branches"`
	Interval   int64    `json:"interval"`
	Enabled    *bool    `json:"enabled"`
	NextRun    string   `json:"next_run"`
	LastStatus string   `json:"last_status"`
}

// queueTask is one observed publication.
type queueTask struct {
	ID         string `json:"id"`
	Target     string `json:"target"`
	Repository string `json:"repository"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

// mirrorLag is what is known about one mirror target.
type mirrorLag struct {
	Status      string
	Lag         *int64
	PublishedAt *string
	Published   map[string]string
	Error       *string
}

// readQueueFile reads the operator's queue. A queue that is not there is not
// an error of the observer: the page falls back to the storage core.
func readQueueFile(path string) (queueFile, error) {
	var queue queueFile
	if path == "" {
		return queue, fmt.Errorf("the path of the mirror queue is not configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return queue, err
	}
	if err := json.Unmarshal(raw, &queue); err != nil {
		return queue, err
	}
	return queue, nil
}

// composeMirrors joins the queue, the measured state and the backup gate
// into the document the mirrors page reads.
func composeMirrors(cfg Config, targets []queueTarget, tasks []queueTask, lag map[string]mirrorLag,
	policy *MirrorPolicy, gate *BackupGateSummary, disk *HostDisk, mtime *string, reason *string, warnings []string, now time.Time) MirrorIntelResponse {

	runsByTarget := map[string][]MirrorRun{}
	for _, task := range tasks {
		run := MirrorRun{
			ID:         task.ID,
			Target:     task.Target,
			Repository: task.Repository,
			Status:     task.Status,
		}
		if run.Status == "" {
			run.Status = "unknown"
		}
		if at := parseInstant(task.CreatedAt); at != nil {
			stamp := isoTime(*at)
			run.QueuedAt = &stamp
		}
		if at := parseInstant(task.StartedAt); at != nil {
			stamp := isoTime(*at)
			run.StartedAt = &stamp
		}
		if at := parseInstant(task.FinishedAt); at != nil {
			stamp := isoTime(*at)
			run.FinishedAt = &stamp
		}
		if run.StartedAt != nil && run.FinishedAt != nil {
			duration := parseInstant(*run.FinishedAt).Sub(*parseInstant(*run.StartedAt)).Milliseconds()
			if duration < 0 {
				duration = 0
			}
			run.DurationMs = &duration
		}
		runsByTarget[task.Target] = append(runsByTarget[task.Target], run)
	}
	for name := range runsByTarget {
		list := runsByTarget[name]
		sort.SliceStable(list, func(i, j int) bool {
			return valueOf(list[i].QueuedAt) > valueOf(list[j].QueuedAt)
		})
		if len(list) > mirrorRunsPerTarget {
			list = list[:mirrorRunsPerTarget]
		}
		runsByTarget[name] = list
	}

	// The window the cadence is judged over: the history the queue holds, at
	// most an hour.
	windowMs := int64(mirrorHistoryWindowMs)
	oldest := now
	for _, task := range tasks {
		at := parseInstant(task.CreatedAt)
		if at == nil {
			continue
		}
		if at.Before(oldest) {
			oldest = *at
		}
	}
	if span := now.Sub(oldest).Milliseconds(); span < windowMs {
		if span < 10*60*1000 {
			span = 10 * 60 * 1000
		}
		windowMs = span
	}

	intel := []MirrorTargetIntel{}
	for _, target := range targets {
		if target.Name == "" || target.Repository == "" || target.URL == "" {
			continue
		}
		interval := int64(defaultIntervalMs)
		if target.Interval > 0 {
			interval = target.Interval / int64(time.Millisecond)
		}
		enabled := target.Enabled == nil || *target.Enabled
		runs := runsByTarget[target.Name]
		observedRuns := int64(len(runs))
		expectedRuns := int64(math.Round(float64(windowMs) / float64(maxInt64(30000, interval))))
		if expectedRuns < 1 {
			expectedRuns = 1
		}
		onSchedule := observedRuns >= maxInt64(1, expectedRuns*8/10)

		gaps := []int64{}
		for i := 1; i < len(runs); i++ {
			newer := parseInstant(valueOf(runs[i-1].QueuedAt))
			older := parseInstant(valueOf(runs[i].QueuedAt))
			if newer != nil && older != nil {
				gaps = append(gaps, newer.Sub(*older).Milliseconds())
			}
		}
		cadence := medianInt64(gaps)

		okRuns := int64(0)
		failStreak := int64(0)
		for _, run := range runs {
			if run.Status == "ok" {
				okRuns++
				continue
			}
			if okRuns == 0 {
				failStreak++
			}
		}
		durations := []int64{}
		for _, run := range runs {
			if run.DurationMs != nil {
				durations = append(durations, *run.DurationMs)
			}
		}
		var avgDuration *int64
		if len(durations) > 0 {
			var sum int64
			for _, value := range durations {
				sum += value
			}
			average := sum / int64(len(durations))
			avgDuration = &average
		}

		info, hasInfo := lag[target.Name]
		lastStatus := target.LastStatus
		if len(runs) > 0 {
			lastStatus = runs[0].Status
		}

		var publishedAt *string
		if hasInfo {
			publishedAt = info.PublishedAt
		}
		lastOk := latestInstant(publishedAt, firstRunOk(runs))
		var lastOkAge *int64
		if lastOk != nil {
			if at := parseInstant(*lastOk); at != nil {
				age := now.Sub(*at).Milliseconds()
				if age < 0 {
					age = 0
				}
				lastOkAge = &age
			}
		}
		var overdue int64
		if enabled && target.NextRun != "" {
			if next := parseInstant(target.NextRun); next != nil && now.After(*next) {
				overdue = now.Sub(*next).Milliseconds()
			}
		}
		var nextRunAt *string
		if next := parseInstant(target.NextRun); next != nil {
			stamp := isoTime(*next)
			nextRunAt = &stamp
		}
		var lastRunAt *string
		if len(runs) > 0 {
			lastRunAt = runs[0].QueuedAt
		}

		published := map[string]string{}
		var syncStatus *string
		var lagValue *int64
		var errorText *string
		if hasInfo {
			published = info.Published
			if published == nil {
				published = map[string]string{}
			}
			if info.Status != "" {
				status := info.Status
				syncStatus = &status
			}
			lagValue = info.Lag
			errorText = info.Error
		}

		entry := MirrorTargetIntel{
			Name:          target.Name,
			Repository:    target.Repository,
			URL:           target.URL,
			Host:          remoteHost(target.URL),
			Branches:      target.Branches,
			Enabled:       enabled,
			IntervalMs:    interval,
			NextRunAt:     nextRunAt,
			OverdueMs:     overdue,
			OnSchedule:    onSchedule,
			CadenceMs:     cadence,
			ExpectedRuns:  expectedRuns,
			ObservedRuns:  observedRuns,
			LastStatus:    lastStatus,
			LastRunAt:     lastRunAt,
			LastOkAt:      lastOk,
			LastOkAgeMs:   lastOkAge,
			FailStreak:    failStreak,
			SuccessRate:   ratio(okRuns, int64(len(runs))),
			AvgDurationMs: avgDuration,
			Lag:           lagValue,
			SyncStatus:    syncStatus,
			PublishedAt:   publishedAt,
			Published:     published,
			Error:         errorText,
			Runs:          runs,
		}
		if len(entry.Branches) == 0 {
			entry.Branches = []string{"*"}
		}
		intel = append(intel, entry)
	}

	sort.SliceStable(intel, func(i, j int) bool {
		if intel[i].Lag == nil && intel[j].Lag != nil {
			return false
		}
		if intel[i].Lag != nil && intel[j].Lag == nil {
			return true
		}
		if intel[i].Lag != nil && intel[j].Lag != nil && *intel[i].Lag != *intel[j].Lag {
			return *intel[i].Lag > *intel[j].Lag
		}
		return intel[i].Name < intel[j].Name
	})

	byRepo := map[string][]MirrorTargetIntel{}
	for _, target := range intel {
		byRepo[target.Repository] = append(byRepo[target.Repository], target)
	}
	repos := []MirrorRepoIntel{}
	for repository, list := range byRepo {
		lags := []int64{}
		lastOks := []*string{}
		for _, target := range list {
			if target.Lag != nil {
				lags = append(lags, *target.Lag)
			}
			if target.LastOkAt != nil {
				lastOks = append(lastOks, target.LastOkAt)
			}
		}
		var worstLag *int64
		if len(lags) > 0 {
			worst := lags[0]
			for _, value := range lags[1:] {
				if value > worst {
					worst = value
				}
			}
			worstLag = &worst
		}
		var lastOkAt *string
		if len(lastOks) > 0 {
			lastOkAt = latestInstant(lastOks...)
		}
		repo := MirrorRepoIntel{
			Repository: repository,
			Branches:   list[0].Branches,
			Targets:    len(list),
			LastOkAt:   lastOkAt,
			WorstLag:   worstLag,
		}
		for _, target := range list {
			switch classifyMirror(target.SyncStatus, target.LastStatus) {
			case "ok":
				repo.Healthy++
			case "failed":
				repo.Failed++
			}
			if !target.OnSchedule {
				repo.Late++
			}
		}
		repos = append(repos, repo)
	}
	sort.SliceStable(repos, func(i, j int) bool {
		left, right := int64(-1), int64(-1)
		if repos[i].WorstLag != nil {
			left = *repos[i].WorstLag
		}
		if repos[j].WorstLag != nil {
			right = *repos[j].WorstLag
		}
		if left != right {
			return left > right
		}
		return repos[i].Repository < repos[j].Repository
	})

	allRuns := []MirrorRun{}
	for _, task := range tasks {
		run := MirrorRun{
			ID:         task.ID,
			Target:     task.Target,
			Repository: task.Repository,
			Status:     task.Status,
		}
		if run.Status == "" {
			run.Status = "unknown"
		}
		if at := parseInstant(task.CreatedAt); at != nil {
			stamp := isoTime(*at)
			run.QueuedAt = &stamp
		}
		if at := parseInstant(task.StartedAt); at != nil {
			stamp := isoTime(*at)
			run.StartedAt = &stamp
		}
		if at := parseInstant(task.FinishedAt); at != nil {
			stamp := isoTime(*at)
			run.FinishedAt = &stamp
		}
		if run.StartedAt != nil && run.FinishedAt != nil {
			duration := parseInstant(*run.FinishedAt).Sub(*parseInstant(*run.StartedAt)).Milliseconds()
			if duration < 0 {
				duration = 0
			}
			run.DurationMs = &duration
		}
		allRuns = append(allRuns, run)
	}
	sort.SliceStable(allRuns, func(i, j int) bool {
		return valueOf(allRuns[i].QueuedAt) > valueOf(allRuns[j].QueuedAt)
	})
	if len(allRuns) > mirrorRecentRuns {
		allRuns = allRuns[:mirrorRecentRuns]
	}

	hosts := []HostDisk{}
	if disk != nil {
		hosts = append(hosts, *disk)
	}

	response := MirrorIntelResponse{
		OK:          reason == nil,
		GeneratedAt: isoTime(now),
		Source: MirrorSource{
			Host:       diskHost(),
			QueuePath:  cfg.QueuePath,
			QueueMtime: mtime,
			LagProbes:  len(lag),
			Error:      reason,
			Warnings:   warnings,
		},
		Policy:  policy,
		Summary: summariseMirrors(intel, allRuns),
		Repos:   repos,
		Targets: intel,
		Runs:    allRuns,
		Backup:  gate,
		Hosts:   hosts,
	}
	return response
}

// summariseMirrors counts the fabric.
func summariseMirrors(targets []MirrorTargetIntel, runs []MirrorRun) MirrorSummary {
	summary := MirrorSummary{Targets: len(targets)}
	for _, target := range targets {
		if target.Enabled {
			summary.Enabled++
		} else {
			summary.Disabled++
		}
		switch classifyMirror(target.SyncStatus, target.LastStatus) {
		case "ok":
			summary.OK++
		case "failed":
			summary.Failed++
		default:
			summary.Idle++
		}
		if target.Enabled && target.OverdueMs > maxInt64(120000, target.IntervalMs/2) {
			summary.Overdue++
		}
		if target.Enabled && (target.LastOkAgeMs == nil || *target.LastOkAgeMs > target.IntervalMs*3) {
			summary.Stale++
		}
		summary.ObservedRuns += target.ObservedRuns
		summary.ExpectedRuns += target.ExpectedRuns
	}
	lags := []int64{}
	for _, target := range targets {
		if target.Lag != nil {
			lags = append(lags, *target.Lag)
		}
	}
	if len(lags) > 0 {
		var sum int64
		for _, value := range lags {
			sum += value
		}
		average := float64(sum) / float64(len(lags))
		rounded := math.Round(average*10) / 10
		summary.AvgLag = &rounded
		maximum := lags[0]
		for _, value := range lags[1:] {
			if value > maximum {
				maximum = value
			}
		}
		summary.MaxLag = &maximum
	}
	ok := int64(0)
	for _, run := range runs {
		if run.Status == "ok" {
			ok++
		}
	}
	summary.SuccessRate = ratio(ok, int64(len(runs)))
	if summary.ExpectedRuns > 0 {
		pct := int64(float64(summary.ObservedRuns) / float64(summary.ExpectedRuns) * 100)
		if pct < 0 {
			pct = 0
		}
		summary.OnSchedulePct = &pct
	}
	times := []*string{}
	for _, run := range runs {
		if run.QueuedAt != nil {
			times = append(times, run.QueuedAt)
		}
	}
	if len(times) > 0 {
		summary.HistoryFrom = latestInstant(times...)
		summary.HistoryTo = oldestInstant(times...)
		summary.LastTaskAt = latestInstant(times...)
	}
	lastOks := []*string{}
	for _, target := range targets {
		if target.LastOkAt != nil {
			lastOks = append(lastOks, target.LastOkAt)
		}
	}
	if len(lastOks) > 0 {
		summary.LastOkAt = latestInstant(lastOks...)
	}
	return summary
}

// classifyMirror says how a mirror target stands.
func classifyMirror(syncStatus *string, lastStatus string) string {
	value := strings.ToLower(firstNonEmpty(valueOf(syncStatus), lastStatus))
	switch value {
	case "ok", "succeeded", "in_sync":
		return "ok"
	case "failed", "error", "diverged", "drifted":
		return "failed"
	}
	return "idle"
}

// readGateReport reads the newest report of the backup gate.
func readGateReport(dir string) *BackupGateSummary {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	newest := ""
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "backup-origins-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil {
			if newest == "" || info.ModTime().After(mustModTime(newest)) {
				newest = path
			}
		}
	}
	if newest == "" {
		return nil
	}
	raw, err := os.ReadFile(newest)
	if err != nil {
		return nil
	}
	var report struct {
		CheckedAt    string `json:"checked_at"`
		Result       string `json:"result"`
		Repositories []struct {
			Repository string `json:"repository"`
			Origin     string `json:"origin"`
			URL        string `json:"url"`
			Status     string `json:"status"`
			Detail     string `json:"detail"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil
	}
	summary := &BackupGateSummary{
		Result:       report.Result,
		Origins:      []BackupOrigin{},
		Repositories: 0,
	}
	if summary.Result == "" {
		summary.Result = "UNKNOWN"
	}
	if at := parseInstant(report.CheckedAt); at != nil {
		stamp := isoTime(*at)
		summary.CheckedAt = &stamp
	}
	seen := map[string]bool{}
	for _, entry := range report.Repositories {
		status := gateStatus(entry.Status)
		origin := BackupOrigin{
			Repository: entry.Repository,
			Origin:     entry.Origin,
			URL:        entry.URL,
			Status:     status,
		}
		if entry.Detail != "" {
			origin.Detail = stringPtr(entry.Detail)
		}
		switch status {
		case BackupInSync:
			summary.InSync++
		case BackupDrifted:
			summary.Drifted++
		case BackupMissing:
			summary.Missing++
		default:
			summary.Failed++
		}
		if entry.Repository != "" && !seen[entry.Repository] {
			seen[entry.Repository] = true
			summary.Repositories++
		}
		summary.Origins = append(summary.Origins, origin)
	}
	return summary
}

// gateStatus maps the word of the gate to a verdict.
func gateStatus(raw string) BackupOriginStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "in_sync", "ok", "match", "matches", "synchronized":
		return BackupInSync
	case "out_of_sync", "drifted", "diverged", "different":
		return BackupDrifted
	case "missing", "absent", "not_found":
		return BackupMissing
	}
	return BackupFailed
}

// readNewestDump reads the newest dump of the storage core.
func readNewestDump(dir string) *BackupDump {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var newest *BackupDump
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dump") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(mustModTime(newest.Path)) {
			dump := &BackupDump{Path: path, SizeBytes: info.Size()}
			stamp := isoTime(info.ModTime())
			dump.At = &stamp
			newest = dump
		}
	}
	return newest
}

// localDisk reads the disk of this host.
func localDisk() *HostDisk {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return nil
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	used := total - free
	usedPct := 0
	if total > 0 {
		usedPct = int(math.Round(float64(used*100) / float64(total)))
	}
	return &HostDisk{
		Host:       diskHost(),
		Role:       "mirror",
		Mount:      "/",
		TotalBytes: total,
		FreeBytes:  free,
		UsedPct:    usedPct,
	}
}

// diskHost is the name of this host.
func diskHost() string {
	name, err := os.Hostname()
	if err != nil {
		return "this host"
	}
	return name
}

// mustModTime reads the modification time of a path.
func mustModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// gateTime is when the gate ran.
func gateTime(gate *BackupGateSummary) *string {
	if gate == nil {
		return nil
	}
	return gate.CheckedAt
}

// firstRunOk is when the last successful run of a target finished.
func firstRunOk(runs []MirrorRun) *string {
	for _, run := range runs {
		if run.Status != "ok" {
			continue
		}
		if run.FinishedAt != nil {
			return run.FinishedAt
		}
		return run.QueuedAt
	}
	return nil
}

// parseInstant reads a timestamp of the queue and of the reports.
func parseInstant(text string) *time.Time {
	if text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return nil
	}
	return &parsed
}

// valueOf is the text of an optional string.
func valueOf(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// isoPtr is the text of an optional instant.
func isoPtr(at *time.Time) *string {
	if at == nil {
		return nil
	}
	stamp := isoTime(*at)
	return &stamp
}

// stringPtr is a pointer to a string.
func stringPtr(value string) *string {
	return &value
}

// firstNonEmpty is the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ratio is the share of a part of a whole.
func ratio(part, total int64) *float64 {
	if total <= 0 {
		return nil
	}
	value := float64(part) / float64(total)
	return &value
}

// medianInt64 is the middle of the values.
func medianInt64(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		value := sorted[middle]
		return &value
	}
	value := int64(math.Round(float64(sorted[middle-1]+sorted[middle]) / 2))
	return &value
}

// maxInt64 is the larger of two numbers.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// latestInstant is the newest of the instants.
func latestInstant(values ...*string) *string {
	var newest *string
	var newestAt time.Time
	for _, value := range values {
		if value == nil {
			continue
		}
		at := parseInstant(*value)
		if at == nil {
			continue
		}
		if newest == nil || at.After(newestAt) {
			newest = value
			newestAt = *at
		}
	}
	return newest
}

// oldestInstant is the oldest of the instants.
func oldestInstant(values ...*string) *string {
	var oldest *string
	var oldestAt time.Time
	for _, value := range values {
		if value == nil {
			continue
		}
		at := parseInstant(*value)
		if at == nil {
			continue
		}
		if oldest == nil || at.Before(oldestAt) {
			oldest = value
			oldestAt = *at
		}
	}
	return oldest
}

// sortedKeys returns the keys of a set, in order.
func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
