package observer

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The CI telemetry of the fleet.
//
// Woodpecker keeps its whole state in PostgreSQL: the pipelines, the
// workflows (the per-agent jobs), the steps and the agents. The honest
// source is that database, not a scraped API, so the observer reads it —
// here, on the server — and hands the rows to the client.
//
// A failing CI database must not take the service down: the last good
// reading is kept and served with ok false, the same contract the mirror
// telemetry uses.

const (
	// ciWindowHours is how far back the summary looks.
	ciWindowHours = 24
	// ciRecentPipelines is how many pipelines are handed to a client.
	ciRecentPipelines = 60
	// ciOnlineWindowMs is how long an agent may be silent to be drawn online.
	ciOnlineWindowMs = 120 * 1000
)

// CiStep is one step of a pipeline.
type CiStep struct {
	PID        int64   `json:"pid"`
	Name       string  `json:"name"`
	State      string  `json:"state"`
	Type       *string `json:"type"`
	ExitCode   *int64  `json:"exitCode"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	DurationMs *int64  `json:"durationMs"`
	Error      *string `json:"error"`
}

// CiWorkflow is one job of a pipeline, with the agent that took it.
type CiWorkflow struct {
	Name       string  `json:"name"`
	State      string  `json:"state"`
	Agent      *string `json:"agent"`
	Platform   *string `json:"platform"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	DurationMs *int64  `json:"durationMs"`
}

// CiPipeline is one execution of a pipeline.
type CiPipeline struct {
	ID         int64        `json:"id"`
	Number     *int64       `json:"number"`
	RepoID     *int64       `json:"repoId"`
	Repo       string       `json:"repo"`
	Branch     *string      `json:"branch"`
	Commit     *string      `json:"commit"`
	Event      *string      `json:"event"`
	Status     string       `json:"status"`
	Author     *string      `json:"author"`
	Message    *string      `json:"message"`
	CreatedAt  *string      `json:"createdAt"`
	StartedAt  *string      `json:"startedAt"`
	FinishedAt *string      `json:"finishedAt"`
	DurationMs *int64       `json:"durationMs"`
	URL        *string      `json:"url"`
	FailedStep *string      `json:"failedStep"`
	Error      *string      `json:"error"`
	Steps      []CiStep     `json:"steps"`
	Workflows  []CiWorkflow `json:"workflows"`
}

// CiRepoRollup counts the runs of one repository.
type CiRepoRollup struct {
	Repo        string   `json:"repo"`
	RepoID      *int64   `json:"repoId"`
	Total       int      `json:"total"`
	Success     int      `json:"success"`
	Failed      int      `json:"failed"`
	Running     int      `json:"running"`
	SuccessRate *float64 `json:"successRate"`
	LastAt      *string  `json:"lastAt"`
	LastStatus  *string  `json:"lastStatus"`
}

// CiAgent is one agent of the CI server.
type CiAgent struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Platform      *string  `json:"platform"`
	Backend       *string  `json:"backend"`
	Capacity      *int64   `json:"capacity"`
	Version       *string  `json:"version"`
	Online        bool     `json:"online"`
	LastContactAt *string  `json:"lastContactAt"`
	Labels        []string `json:"labels"`
}

// CiSummary counts the executions.
type CiSummary struct {
	AllTime       int      `json:"allTime"`
	Total         int      `json:"total"`
	Success       int      `json:"success"`
	Failure       int      `json:"failure"`
	Running       int      `json:"running"`
	Pending       int      `json:"pending"`
	SuccessRate   *float64 `json:"successRate"`
	AvgDurationMs *int64   `json:"avgDurationMs"`
	P50DurationMs *int64   `json:"p50DurationMs"`
	WindowHours   int      `json:"windowHours"`
}

// CiHourBucket is one hour of the histogram.
type CiHourBucket struct {
	At     string `json:"at"`
	Total  int    `json:"total"`
	Failed int    `json:"failed"`
}

// CiSource says where the CI telemetry comes from.
type CiSource struct {
	Database *string `json:"database"`
	URL      *string `json:"url"`
	Error    *string `json:"error"`
}

// CiResponse is what the CI page reads.
type CiResponse struct {
	OK          bool           `json:"ok"`
	GeneratedAt string         `json:"generatedAt"`
	CachedForMs int64          `json:"cachedForMs"`
	Source      CiSource       `json:"source"`
	Summary     CiSummary      `json:"summary"`
	Hourly      []CiHourBucket `json:"hourly"`
	Repos       []CiRepoRollup `json:"repos"`
	Pipelines   []CiPipeline   `json:"pipelines"`
	Agents      []CiAgent      `json:"agents"`
}

// CIIntel reads the CI telemetry.
type CIIntel struct {
	cfg Config

	mu       sync.Mutex
	cached   *ciValue
	inflight chan struct{}
}

// ciValue is one collected reading.
type ciValue struct {
	at       time.Time
	response CiResponse
}

// NewCIIntel returns the collector of the CI telemetry.
func NewCIIntel(cfg Config) *CIIntel {
	return &CIIntel{cfg: cfg}
}

// Get answers with the telemetry. A reading that is younger than the
// configured age is served as it is; an older one is served while it is
// collected again.
func (c *CIIntel) Get(ctx context.Context) CiResponse {
	now := time.Now()
	c.mu.Lock()
	cached := c.cached
	c.mu.Unlock()

	if cached != nil && now.Sub(cached.at) < c.cfg.CITTL {
		response := cached.response
		response.CachedForMs = now.Sub(cached.at).Milliseconds()
		return response
	}
	if cached != nil {
		go c.refresh(context.WithoutCancel(ctx))
		response := cached.response
		response.CachedForMs = now.Sub(cached.at).Milliseconds()
		return response
	}

	// Nothing has been collected yet: the first reading has to be waited
	// for.
	select {
	case <-c.refresh(ctx):
	case <-time.After(30 * time.Second):
	}
	c.mu.Lock()
	value := c.cached
	c.mu.Unlock()
	if value == nil {
		return c.empty("the CI telemetry could not be collected")
	}
	return value.response
}

// refresh collects the telemetry once. It returns the channel that is
// closed when the collection is done, so that a caller that needs the first
// reading waits for it and the others do not collect twice.
func (c *CIIntel) refresh(ctx context.Context) <-chan struct{} {
	c.mu.Lock()
	if c.inflight != nil {
		done := c.inflight
		c.mu.Unlock()
		return done
	}
	done := make(chan struct{})
	c.inflight = done
	c.mu.Unlock()

	go func() {
		defer close(done)
		defer func() {
			c.mu.Lock()
			c.inflight = nil
			c.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		response, err := c.collect(ctx)
		if err != nil {
			c.mu.Lock()
			cached := c.cached
			c.mu.Unlock()
			if cached != nil {
				stale := cached.response
				stale.OK = false
				stale.Source.Error = stringPtr("the CI telemetry is unavailable: " + err.Error())
				c.mu.Lock()
				c.cached = &ciValue{at: cached.at, response: stale}
				c.mu.Unlock()
				return
			}
			response = c.empty("the CI telemetry is unavailable: " + err.Error())
		}
		c.mu.Lock()
		c.cached = &ciValue{at: time.Now(), response: response}
		c.mu.Unlock()
	}()
	return done
}

// empty answers with nothing, and the reason why.
func (c *CIIntel) empty(reason string) CiResponse {
	return CiResponse{
		OK:          false,
		GeneratedAt: isoTime(time.Now()),
		Source: CiSource{
			Database: optional(describeDatabase(c.cfg.CIDatabase)),
			URL:      optional(c.cfg.CIURL),
			Error:    stringPtr(reason),
		},
		Summary:   CiSummary{WindowHours: ciWindowHours},
		Hourly:    []CiHourBucket{},
		Repos:     []CiRepoRollup{},
		Pipelines: []CiPipeline{},
		Agents:    []CiAgent{},
	}
}

// collect reads the CI database.
func (c *CIIntel) collect(ctx context.Context) (CiResponse, error) {
	if c.cfg.CIDatabase == "" {
		return c.empty("no CI database is configured"), nil
	}
	db, err := sql.Open("pgx", c.cfg.CIDatabase)
	if err != nil {
		return CiResponse{}, fmt.Errorf("open the CI database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return CiResponse{}, fmt.Errorf("the CI database does not answer: %w", err)
	}

	since := time.Now().Add(-ciWindowHours * time.Hour).Unix()

	pipelines, err := c.query(ctx, db, `
		select p.id, p.repo_id, coalesce(r.full_name, 'repo ' || p.repo_id) as repo, p.number, p.status,
		       p.event, p.branch, p.commit, p.author, p.message, p.created, p.started, p.finished, p.errors
		  from pipelines p left join repos r on r.id = p.repo_id
		 order by p.id desc limit $1`, ciRecentPipelines)
	if err != nil {
		return CiResponse{}, err
	}
	steps, err := c.query(ctx, db, `
		select pipeline_id, pid, name, state, type, exit_code, started, finished, error
		  from steps
		 where pipeline_id in (select id from pipelines order by id desc limit $1)
		 order by pipeline_id desc, pid asc`, ciRecentPipelines)
	if err != nil {
		return CiResponse{}, err
	}
	workflows, err := c.query(ctx, db, `
		select w.pipeline_id, w.name, w.state, w.platform, w.started, w.finished, a.name as agent
		  from workflows w left join agents a on a.id = w.agent_id
		 where w.pipeline_id in (select id from pipelines order by id desc limit $1)
		 order by w.pipeline_id desc, w.pid asc`, ciRecentPipelines)
	if err != nil {
		return CiResponse{}, err
	}
	agents, err := c.query(ctx, db, `
		select id, name, platform, backend, capacity, version, last_contact, custom_labels
		  from agents order by name`)
	if err != nil {
		return CiResponse{}, err
	}
	totals, err := c.query(ctx, db, `
		select count(*)::int as all_time,
		       count(*) filter (where created >= $1)::int as total,
		       count(*) filter (where created >= $1 and status = 'success')::int as success,
		       count(*) filter (where created >= $1 and status = 'failure')::int as failure,
		       count(*) filter (where created >= $1 and status = 'running')::int as running,
		       count(*) filter (where created >= $1 and status in ('pending', 'started', 'blocked', 'created'))::int as pending,
		       avg((finished - started) * 1000.0) filter (where created >= $1 and started > 0 and finished > started) as avg_duration,
		       percentile_cont(0.5) within group (order by (finished - started) * 1000.0)
		         filter (where created >= $1 and started > 0 and finished > started) as p50_duration
		  from pipelines`, since)
	if err != nil {
		return CiResponse{}, err
	}
	byRepo, err := c.query(ctx, db, `
		select p.repo_id, coalesce(r.full_name, 'repo ' || p.repo_id) as repo,
		       count(*)::int as total,
		       count(*) filter (where p.status = 'success')::int as success,
		       count(*) filter (where p.status = 'failure')::int as failure,
		       count(*) filter (where p.status = 'running')::int as running,
		       max(p.created) as last_created
		  from pipelines p left join repos r on r.id = p.repo_id
		 where p.created >= $1
		 group by 1, 2
		 order by total desc, repo asc`, since)
	if err != nil {
		return CiResponse{}, err
	}
	hourly, err := c.query(ctx, db, `
		select (created / 3600) * 3600 as bucket, count(*)::int as total,
		       count(*) filter (where status = 'failure')::int as failed
		  from pipelines
		 where created >= $1
		 group by 1 order by 1`, since)
	if err != nil {
		return CiResponse{}, err
	}
	lastByRepo, err := c.query(ctx, db, `
		select distinct on (p.repo_id) p.repo_id, p.status, p.created
		  from pipelines p
		 order by p.repo_id, p.id desc`)
	if err != nil {
		return CiResponse{}, err
	}

	stepsByPipeline := map[int64][]CiStep{}
	for _, row := range steps {
		pipelineID, ok := row["pipeline_id"].(int64)
		if !ok {
			continue
		}
		step := CiStep{
			PID:   numberOf(row["pid"]),
			Name:  textOf(row["name"], "step"),
			State: textOf(row["state"], "unknown"),
		}
		if value, ok := row["type"].(string); ok {
			step.Type = &value
		}
		if value, ok := row["exit_code"].(int64); ok {
			exit := value
			step.ExitCode = &exit
		}
		step.StartedAt = instantOf(row["started"])
		step.FinishedAt = instantOf(row["finished"])
		step.DurationMs = spanOf(row["started"], row["finished"])
		if value, ok := row["error"].(string); ok && value != "" {
			step.Error = &value
		}
		stepsByPipeline[pipelineID] = append(stepsByPipeline[pipelineID], step)
	}

	workflowsByPipeline := map[int64][]CiWorkflow{}
	for _, row := range workflows {
		pipelineID, ok := row["pipeline_id"].(int64)
		if !ok {
			continue
		}
		workflow := CiWorkflow{
			Name:  textOf(row["name"], "workflow"),
			State: textOf(row["state"], "unknown"),
		}
		if value, ok := row["agent"].(string); ok {
			workflow.Agent = &value
		}
		if value, ok := row["platform"].(string); ok {
			workflow.Platform = &value
		}
		workflow.StartedAt = instantOf(row["started"])
		workflow.FinishedAt = instantOf(row["finished"])
		workflow.DurationMs = spanOf(row["started"], row["finished"])
		workflowsByPipeline[pipelineID] = append(workflowsByPipeline[pipelineID], workflow)
	}

	recent := []CiPipeline{}
	for _, row := range pipelines {
		id := numberOf(row["id"])
		repoID, hasRepo := row["repo_id"].(int64)
		pipeline := CiPipeline{
			ID:         id,
			Repo:       textOf(row["repo"], "unknown"),
			Status:     textOf(row["status"], "unknown"),
			CreatedAt:  instantOf(row["created"]),
			StartedAt:  instantOf(row["started"]),
			FinishedAt: instantOf(row["finished"]),
			DurationMs: spanOf(row["started"], row["finished"]),
		}
		if hasRepo {
			value := repoID
			pipeline.RepoID = &value
		}
		if value, ok := row["number"].(int64); ok {
			number := value
			pipeline.Number = &number
		}
		for _, key := range []struct {
			column string
			target **string
		}{
			{"branch", &pipeline.Branch},
			{"commit", &pipeline.Commit},
			{"event", &pipeline.Event},
			{"author", &pipeline.Author},
			{"message", &pipeline.Message},
		} {
			if value, ok := row[key.column].(string); ok {
				text := value
				*key.target = &text
			}
		}
		pipelineSteps := stepsByPipeline[id]
		pipeline.Steps = pipelineSteps
		if pipeline.Steps == nil {
			pipeline.Steps = []CiStep{}
		}
		pipeline.Workflows = workflowsByPipeline[id]
		if pipeline.Workflows == nil {
			pipeline.Workflows = []CiWorkflow{}
		}
		var failedStep *CiStep
		for i := range pipelineSteps {
			state := pipelineSteps[i].State
			if state != "success" && state != "running" && state != "pending" && state != "skipped" {
				failedStep = &pipelineSteps[i]
				break
			}
		}
		if failedStep != nil {
			pipeline.FailedStep = stringPtr(failedStep.Name)
		}
		if value, ok := row["errors"].(string); ok {
			if message := firstCIError(value); message != "" {
				pipeline.Error = &message
			}
		}
		if pipeline.Error == nil && failedStep != nil && failedStep.Error != nil {
			pipeline.Error = failedStep.Error
		}
		if c.cfg.CIURL != "" && pipeline.RepoID != nil && pipeline.Number != nil {
			pipeline.URL = stringPtr(fmt.Sprintf("%s/repos/%d/pipeline/%d",
				strings.TrimRight(c.cfg.CIURL, "/"), *pipeline.RepoID, *pipeline.Number))
		}
		recent = append(recent, pipeline)
	}

	lastStatus := map[int64]struct {
		status string
		at     *string
	}{}
	for _, row := range lastByRepo {
		repoID, ok := row["repo_id"].(int64)
		if !ok {
			continue
		}
		lastStatus[repoID] = struct {
			status string
			at     *string
		}{status: textOf(row["status"], "unknown"), at: instantOf(row["created"])}
	}

	repos := []CiRepoRollup{}
	for _, row := range byRepo {
		rollup := CiRepoRollup{
			Repo:    textOf(row["repo"], "unknown"),
			Total:   int(numberOf(row["total"])),
			Success: int(numberOf(row["success"])),
			Failed:  int(numberOf(row["failure"])),
			Running: int(numberOf(row["running"])),
		}
		if repoID, ok := row["repo_id"].(int64); ok {
			value := repoID
			rollup.RepoID = &value
			if last, ok := lastStatus[repoID]; ok {
				rollup.LastAt = last.at
				rollup.LastStatus = stringPtr(last.status)
			}
		}
		rollup.SuccessRate = ratio(int64(rollup.Success), int64(rollup.Success+rollup.Failed))
		repos = append(repos, rollup)
	}

	summary := CiSummary{WindowHours: ciWindowHours}
	if len(totals) > 0 {
		row := totals[0]
		summary.AllTime = int(numberOf(row["all_time"]))
		summary.Total = int(numberOf(row["total"]))
		summary.Success = int(numberOf(row["success"]))
		summary.Failure = int(numberOf(row["failure"]))
		summary.Running = int(numberOf(row["running"]))
		summary.Pending = int(numberOf(row["pending"]))
		summary.SuccessRate = ratio(int64(summary.Success), int64(summary.Success+summary.Failure))
		if value, ok := row["avg_duration"].(float64); ok {
			duration := int64(value + 0.5)
			summary.AvgDurationMs = &duration
		}
		if value, ok := row["p50_duration"].(float64); ok {
			duration := int64(value + 0.5)
			summary.P50DurationMs = &duration
		}
	}

	buckets := []CiHourBucket{}
	for _, row := range hourly {
		bucket := CiHourBucket{
			Total:  int(numberOf(row["total"])),
			Failed: int(numberOf(row["failed"])),
		}
		if value, ok := row["bucket"].(int64); ok {
			bucket.At = isoTime(time.Unix(value, 0).UTC())
		}
		buckets = append(buckets, bucket)
	}

	agentRows := []CiAgent{}
	now := time.Now()
	for _, row := range agents {
		agent := CiAgent{
			ID:     numberOf(row["id"]),
			Name:   textOf(row["name"], "agent"),
			Labels: []string{},
			Online: false,
		}
		if value, ok := row["platform"].(string); ok {
			agent.Platform = &value
		}
		if value, ok := row["backend"].(string); ok {
			agent.Backend = &value
		}
		if value, ok := row["capacity"].(int64); ok {
			capacity := value
			agent.Capacity = &capacity
		}
		if value, ok := row["version"].(string); ok {
			agent.Version = &value
		}
		lastContact := instantOf(row["last_contact"])
		agent.LastContactAt = lastContact
		if lastContact != nil {
			if at, err := time.Parse(time.RFC3339, *lastContact); err == nil {
				agent.Online = now.Sub(at) < ciOnlineWindowMs
			}
		}
		switch labels := row["custom_labels"].(type) {
		case []any:
			for _, entry := range labels {
				agent.Labels = append(agent.Labels, fmt.Sprintf("%v", entry))
			}
		case map[string]any:
			for key := range labels {
				agent.Labels = append(agent.Labels, key)
			}
			sort.Strings(agent.Labels)
		}
		agentRows = append(agentRows, agent)
	}

	return CiResponse{
		OK:          true,
		GeneratedAt: isoTime(now),
		Source: CiSource{
			Database: optional(describeDatabase(c.cfg.CIDatabase)),
			URL:      optional(c.cfg.CIURL),
		},
		Summary:   summary,
		Hourly:    buckets,
		Repos:     repos,
		Pipelines: recent,
		Agents:    agentRows,
	}, nil
}

// query runs a query of the CI database.
func (c *CIIntel) query(ctx context.Context, db *sql.DB, statement string, args ...any) ([]map[string]any, error) {
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read the CI database: %w", err)
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		holders := make([]any, len(columns))
		for i := range values {
			holders[i] = &values[i]
		}
		if err := rows.Scan(holders...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, column := range columns {
			row[column] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// numberOf reads a number of a row.
func numberOf(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int32:
		return int64(typed)
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}
	return 0
}

// textOf reads the text of a row, with a fallback.
func textOf(value any, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}

// instantOf reads a timestamp of a row. Woodpecker keeps them in seconds.
func instantOf(value any) *string {
	switch typed := value.(type) {
	case int64:
		if typed <= 0 {
			return nil
		}
		stamp := isoTime(time.Unix(typed, 0).UTC())
		return &stamp
	case float64:
		if typed <= 0 {
			return nil
		}
		stamp := isoTime(time.Unix(int64(typed), 0).UTC())
		return &stamp
	}
	return nil
}

// spanOf reads the duration between two timestamps of a row.
func spanOf(start, finish any) *int64 {
	from, ok1 := start.(int64)
	to, ok2 := finish.(int64)
	if !ok1 || !ok2 || from <= 0 || to <= 0 || to < from {
		return nil
	}
	duration := (to - from) * 1000
	return &duration
}

// firstCIError reads the first error of a pipeline.
func firstCIError(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed
	}
	return trimmed
}
