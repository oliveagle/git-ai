package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The names of the galaxies.
//
// The rules of the client can tell a family together but they only have token
// counts to name it with, which is how a dozen unrelated bulk imports end up
// called "import" and "import 2". A model reading the member list, the stacks,
// the agents and the mirror host can say what the family actually is.
//
// The model is called from here, never from the browser: the cost is the
// design, because a fleet map is stable for weeks.
//
//   - a name is keyed by the sorted member ids, so an unchanged galaxy is
//     never asked about twice — not on restart, not on reload, not on a
//     reorder;
//   - a change of membership is the only trigger;
//   - one family per request, because a twelve-family prompt made every
//     reasoning model burn its whole token budget thinking and answer
//     nothing, while a single-family prompt comes back in two seconds with
//     no reasoning;
//   - a small gap between the requests and a hard daily ceiling sit above
//     that;
//   - a failure is remembered per signature and not retried for hours;
//   - with no model configured nothing is ever called and the rules stand.

const (
	// labelBudget is how many tokens a naming call may use. The answer is
	// two short lines: anything more is a model that thinks out loud.
	labelBudget = 800
	// labelBudgetRetry is the ceiling of the second try. A model that spent
	// the first one thinking needs room to answer, and a label is worth a
	// few hundred tokens.
	labelBudgetRetry = 3200
)

// errLabelBudget says the model ran out of room before it answered.
var errLabelBudget = errors.New("the model ran out of output budget")

// LabelTarget is one channel of the naming model.
type LabelTarget struct {
	// URL is the full endpoint, e.g. http://host:3795/v1/responses/deepseek.
	URL string `json:"url"`
	// Model is the name the endpoint expects.
	Model string `json:"model"`
}

// GalaxyFamily is what a client asks to have named.
type GalaxyFamily struct {
	// Signature is the sorted member ids: the key a name is cached under.
	Signature string `json:"signature"`
	// Members are the repositories of the family.
	Members []string `json:"members"`
	// Stacks are the technologies the members are written in.
	Stacks []string `json:"stacks"`
	// Agents are the actors that work on the members.
	Agents []string `json:"agents"`
	// Hosts are the mirrors the members are published to.
	Hosts []string `json:"hosts"`
	// RuleName is the label the rules gave the family.
	RuleName string `json:"ruleName"`
}

// GalaxyName is one model-written label.
type GalaxyName struct {
	Signature string `json:"signature"`
	Name      string `json:"name"`
	Summary   string `json:"summary"`
	Model     string `json:"model"`
	At        string `json:"at"`
}

// GalaxyCall is one call of the model, kept as the meter of the budget.
type GalaxyCall struct {
	At    string `json:"at"`
	OK    bool   `json:"ok"`
	Model string `json:"model"`
	Error string `json:"error,omitempty"`
}

// GalaxyStatus is the meter of the namer.
type GalaxyStatus struct {
	Configured bool         `json:"configured"`
	Model      string       `json:"model"`
	Names      []GalaxyName `json:"names"`
	Pending    int          `json:"pending"`
	CallsToday int          `json:"callsToday"`
	DailyLimit int          `json:"dailyLimit"`
	LastCallAt *string      `json:"lastCallAt"`
	LastError  *string      `json:"lastError"`
}

const (
	// galaxyCacheVersion is the version of the cache file.
	galaxyCacheVersion = 1
	// maxGalaxyName is how long a name may be.
	maxGalaxyName = 48
	// maxGalaxySummary is how long a summary may be.
	maxGalaxySummary = 96
)

// galaxyCacheFile is the cache of the names, kept beside the journal of the
// events so a restart does not ask the model again.
type galaxyCacheFile struct {
	Version int                   `json:"version"`
	Names   map[string]GalaxyName `json:"names"`
	Retry   map[string]string     `json:"retry"`
	Calls   []GalaxyCall          `json:"calls"`
}

// GalaxyNamer names the galaxies of the fleet, within a budget.
type GalaxyNamer struct {
	cfg  Config
	file string

	mu       sync.Mutex
	cache    galaxyCacheFile
	pending  []GalaxyFamily
	lastCall time.Time
	lastErr  string
	wake     chan struct{}
	stop     chan struct{}
	once     sync.Once
}

// NewGalaxyNamer returns the namer of the configuration.
func NewGalaxyNamer(cfg Config) *GalaxyNamer {
	file := ""
	if cfg.StateDir != "" {
		file = filepath.Join(cfg.StateDir, "galaxy-names.json")
	}
	namer := &GalaxyNamer{
		cfg:  cfg,
		file: file,
		cache: galaxyCacheFile{
			Version: galaxyCacheVersion,
			Names:   map[string]GalaxyName{},
			Retry:   map[string]string{},
		},
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	namer.load()
	return namer
}

// Start runs the worker that names the families one at a time.
func (n *GalaxyNamer) Start() {
	go n.run()
}

// Stop ends the worker.
func (n *GalaxyNamer) Stop() {
	n.once.Do(func() { close(n.stop) })
}

// Configured tells whether a model is configured at all.
func (n *GalaxyNamer) Configured() bool {
	return len(n.cfg.LabelTargets) > 0
}

// Request asks for the families to be named. A family that is already named,
// or that failed recently, or that the daily ceiling does not allow, costs
// nothing.
func (n *GalaxyNamer) Request(families []GalaxyFamily) GalaxyStatus {
	n.mu.Lock()
	for _, family := range families {
		if family.Signature == "" {
			continue
		}
		if _, known := n.cache.Names[family.Signature]; known {
			continue
		}
		if until, waiting := n.cache.Retry[family.Signature]; waiting {
			if at, err := time.Parse(time.RFC3339, until); err == nil && time.Now().Before(at) {
				continue
			}
			delete(n.cache.Retry, family.Signature)
		}
		if n.queued(family.Signature) {
			continue
		}
		n.pending = append(n.pending, family)
	}
	n.mu.Unlock()
	n.wakeWorker()
	return n.Status()
}

// queued tells whether a signature is already waiting to be named.
func (n *GalaxyNamer) queued(signature string) bool {
	for _, family := range n.pending {
		if family.Signature == signature {
			return true
		}
	}
	return false
}

// Names are the labels the model wrote.
func (n *GalaxyNamer) Names() []GalaxyName {
	n.mu.Lock()
	defer n.mu.Unlock()
	names := make([]GalaxyName, 0, len(n.cache.Names))
	for _, entry := range n.cache.Names {
		names = append(names, entry)
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Signature < names[j].Signature })
	return names
}

// Status is the meter of the namer.
func (n *GalaxyNamer) Status() GalaxyStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	status := GalaxyStatus{
		Configured: n.Configured(),
		Names:      make([]GalaxyName, 0, len(n.cache.Names)),
		Pending:    len(n.pending),
		CallsToday: n.callsToday(),
		DailyLimit: n.cfg.LabelDailyCalls,
		LastError:  optional(n.lastErr),
	}
	for _, entry := range n.cache.Names {
		status.Names = append(status.Names, entry)
	}
	sort.Slice(status.Names, func(i, j int) bool { return status.Names[i].Signature < status.Names[j].Signature })
	if len(n.cfg.LabelTargets) > 0 {
		status.Model = n.cfg.LabelTargets[0].Model
	}
	if !n.lastCall.IsZero() {
		status.LastCallAt = optional(isoTime(n.lastCall))
	}
	return status
}

// callsToday counts the calls of the day of the timestamps. The caller
// holds the lock.
func (n *GalaxyNamer) callsToday() int {
	today := dayKey(time.Now())
	count := 0
	for _, call := range n.cache.Calls {
		if strings.HasPrefix(call.At, today) {
			count++
		}
	}
	return count
}

// wakeWorker nudges the worker.
func (n *GalaxyNamer) wakeWorker() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// run names the pending families, one at a time.
func (n *GalaxyNamer) run() {
	for {
		select {
		case <-n.stop:
			return
		case <-n.wake:
			n.drain()
		}
	}
}

// drain names the pending families until there is nothing left to name.
func (n *GalaxyNamer) drain() {
	for {
		family, ok := n.take()
		if !ok {
			return
		}
		if err := n.name(family); err != nil {
			n.mu.Lock()
			n.lastErr = err.Error()
			n.mu.Unlock()
		}
	}
}

// take picks the next family to name. It refuses when the budget of the day
// is spent; the family stays pending for tomorrow.
func (n *GalaxyNamer) take() (GalaxyFamily, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for len(n.pending) > 0 {
		family := n.pending[0]
		n.pending = n.pending[1:]
		if _, known := n.cache.Names[family.Signature]; known {
			continue
		}
		if n.cfg.LabelDailyCalls > 0 && n.callsToday() >= n.cfg.LabelDailyCalls {
			// Put it back: the ceiling is a budget, not a decision.
			n.pending = append([]GalaxyFamily{family}, n.pending...)
			return GalaxyFamily{}, false
		}
		if !n.lastCall.IsZero() {
			gap := n.cfg.LabelMinInterval - time.Since(n.lastCall)
			if gap > 0 {
				n.pending = append([]GalaxyFamily{family}, n.pending...)
				time.AfterFunc(gap, n.wakeWorker)
				return GalaxyFamily{}, false
			}
		}
		return family, true
	}
	return GalaxyFamily{}, false
}

// name asks the model for the label of one family.
// name asks the model for the label of one family.
func (n *GalaxyNamer) name(family GalaxyFamily) error {
	if !n.Configured() {
		return fmt.Errorf("no naming model is configured")
	}
	startedAt := time.Now()
	input := galaxyPrompt(family)
	var failures []string

	for _, target := range n.cfg.LabelTargets {
		// The first call asks for a small answer. A model that spends the
		// whole ceiling thinking is given room once: the label is worth a
		// few hundred tokens, and giving up is worse.
		budget := labelBudget
		strict := false
		for round := 0; round < 3; round++ {
			text, err := n.call(target, input, strict, budget)
			if errors.Is(err, errLabelBudget) {
				failures = append(failures, fmt.Sprintf("%s: the model ran out of budget", target.Model))
				if budget < labelBudgetRetry {
					budget = labelBudgetRetry
					continue
				}
				break
			}
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %s", target.Model, truncate(err.Error(), 160)))
				// Only a bad label is worth a second try; a rejected or
				// missing channel would just burn another attempt.
				if !strings.Contains(err.Error(), "no label") {
					break
				}
				strict = true
				continue
			}
			label, summary := parseGalaxyLabel(text)
			if !isPlausibleLabel(label) {
				failures = append(failures, fmt.Sprintf("%s: no label", target.Model))
				strict = true
				continue
			}
			n.mu.Lock()
			n.cache.Names[family.Signature] = GalaxyName{
				Signature: family.Signature,
				Name:      label,
				Summary:   summary,
				Model:     target.Model,
				At:        isoTime(time.Now()),
			}
			delete(n.cache.Retry, family.Signature)
			n.lastCall = time.Now()
			n.lastErr = ""
			n.cache.Calls = append(n.cache.Calls, GalaxyCall{
				At:    isoTime(startedAt),
				OK:    true,
				Model: target.Model,
			})
			n.trimCalls()
			n.mu.Unlock()
			n.persist()
			return nil
		}
	}

	reason := strings.Join(failures, " | ")
	if reason == "" {
		reason = "no target answered"
	}
	n.mu.Lock()
	n.cache.Retry[family.Signature] = isoTime(time.Now().Add(n.cfg.LabelRetryAfter))
	n.lastErr = truncate(reason, 300)
	n.lastCall = time.Now()
	n.cache.Calls = append(n.cache.Calls, GalaxyCall{
		At:    isoTime(startedAt),
		OK:    false,
		Model: n.cfg.LabelTargets[0].Model,
		Error: n.lastErr,
	})
	n.trimCalls()
	n.mu.Unlock()
	n.persist()
	return fmt.Errorf("%s", reason)
}

// trimCalls keeps the meter small.
func (n *GalaxyNamer) trimCalls() {
	if len(n.cache.Calls) > 200 {
		n.cache.Calls = n.cache.Calls[len(n.cache.Calls)-200:]
	}
}

// call asks one channel of the model.
// call asks one channel of the model.
//
// The budget is the ceiling of the answer. A model that thinks out loud
// spends it and answers nothing: that is errLabelBudget, and the caller
// may give it more room.
func (n *GalaxyNamer) call(target LabelTarget, input string, strict bool, budget int) (string, error) {
	prompt := input
	if strict {
		prompt += "\n\nYour last answer was rejected: it must be exactly two short lines, the name and the summary, with no explanation and no reasoning."
	}
	body, err := json.Marshal(map[string]any{
		"model": target.Model,
		"input": prompt,
		// Two short lines: a reasoning model that spends its budget thinking
		// is the failure mode this ceiling exists to expose.
		"max_output_tokens": budget,
		"reasoning":         map[string]any{"effort": "low"},
	})
	if err != nil {
		return "", err
	}
	timeout := n.cfg.LabelTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if n.cfg.LabelKey != "" {
		request.Header.Set("Authorization", "Bearer "+n.cfg.LabelKey)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, truncate(string(raw), 160))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("not JSON: %s", truncate(string(raw), 160))
	}
	if reason := incompleteReason(payload); reason != "" {
		if reason == "max_output_tokens" {
			return "", fmt.Errorf("%w: %s", errLabelBudget, reason)
		}
		return "", fmt.Errorf("the answer is incomplete: %s", reason)
	}
	text := modelText(payload)
	if text == "" {
		return "", fmt.Errorf("no label: %s", truncate(string(raw), 160))
	}
	return text, nil
}

// incompleteReason says why an answer stopped short, if it did. An empty
// answer is not the same as an answer that ran out of room.
func incompleteReason(payload map[string]any) string {
	status, _ := payload["status"].(string)
	if status != "incomplete" {
		return ""
	}
	details, ok := payload["incomplete_details"].(map[string]any)
	if !ok {
		return "unknown"
	}
	reason, _ := details["reason"].(string)
	if reason == "" {
		return "unknown"
	}
	return reason
}

// galaxyPrompt is the question asked about one family.
func galaxyPrompt(family GalaxyFamily) string {
	lines := []string{fmt.Sprintf("Repositories: %s.", strings.Join(limitStrings(family.Members, 10), ", "))}
	if len(family.Stacks) > 0 {
		lines = append(lines, fmt.Sprintf("Stack: %s.", strings.Join(family.Stacks, ", ")))
	}
	if len(family.Agents) > 0 {
		lines = append(lines, fmt.Sprintf("Agents: %s.", strings.Join(family.Agents, ", ")))
	}
	if len(family.Hosts) > 0 {
		lines = append(lines, fmt.Sprintf("Mirrors: %s.", strings.Join(family.Hosts, ", ")))
	}
	if family.RuleName != "" {
		lines = append(lines, fmt.Sprintf("A rule-based label said %q.", family.RuleName))
	}
	lines = append(lines,
		"",
		"Name this group in 2-4 lowercase words (at most 32 characters) that say what it is:",
		"its stack or domain plus its purpose. Never a bare host name, never a number, never",
		`"misc" or "various". Then, on the next line, one sentence under 80 characters saying`,
		"what the members have in common and what they do.",
		"Two lines only, no quotes, no JSON, no preamble. Do not explain, do not think out",
		"loud, do not describe the repositories before naming them.",
	)
	return strings.Join(lines, "\n")
}

// modelText reads the answer of a model, whether it answers in the Responses
// shape or in the chat-completions shape.
func modelText(payload map[string]any) string {
	if text, ok := payload["output_text"].(string); ok && strings.TrimSpace(text) != "" {
		return text
	}
	if output, ok := payload["output"].([]any); ok {
		var parts []string
		for _, item := range output {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := entry["content"].([]any)
			if !ok {
				continue
			}
			for _, piece := range content {
				part, ok := piece.(map[string]any)
				if !ok {
					continue
				}
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
		choice, ok := choices[0].(map[string]any)
		if !ok {
			return ""
		}
		message, ok := choice["message"].(map[string]any)
		if !ok {
			return ""
		}
		content, _ := message["content"].(string)
		return content
	}
	return ""
}

// galaxyMetaWords are the words of a model that is thinking out loud.
var galaxyMetaWords = regexp.MustCompile(`(?i)\b(we|i|let|need|user|answer|request|given|analysis|json|produce|final|okay)\b`)

// parseGalaxyLabel reads the two lines of the answer: the name and the
// summary.
func parseGalaxyLabel(text string) (string, string) {
	cleaned := strings.ReplaceAll(text, "```json", "")
	cleaned = strings.ReplaceAll(cleaned, "```text", "")
	cleaned = strings.ReplaceAll(cleaned, "```", "")
	lines := []string{}
	for _, line := range strings.Split(cleaned, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = regexp.MustCompile(`^[-*\d.\s]+`).ReplaceAllString(line, "")
		line = regexp.MustCompile(`(?i)^(name|label|summary|description)\s*[:：]\s*`).ReplaceAllString(line, "")
		line = strings.Trim(line, "\"'")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if regexp.MustCompile(`(?i)^(here|sure|certainly|the group|this group)`).MatchString(line) {
			continue
		}
		lines = append(lines, line)
	}
	for index, line := range lines {
		if !isPlausibleLabel(line) {
			continue
		}
		name := collapseSpaces(line)
		if len(name) > maxGalaxyName {
			name = name[:maxGalaxyName]
		}
		summary := ""
		if index+1 < len(lines) {
			summary = collapseSpaces(lines[index+1])
			if len(summary) > maxGalaxySummary {
				summary = summary[:maxGalaxySummary]
			}
		}
		return name, summary
	}
	fallback := ""
	for _, line := range lines {
		if len(line) <= maxGalaxyName {
			fallback = collapseSpaces(line)
		}
	}
	if len(fallback) > maxGalaxyName {
		fallback = fallback[:maxGalaxyName]
	}
	return fallback, ""
}

// isPlausibleLabel tells a label from a sentence a model narrated.
func isPlausibleLabel(name string) bool {
	if len(name) < 3 || len(name) > 40 {
		return false
	}
	if strings.ContainsAny(name[len(name)-1:], ".:;!?") {
		return false
	}
	if galaxyMetaWords.MatchString(name) {
		return false
	}
	words := strings.Fields(name)
	return len(words) >= 2 && len(words) <= 6
}

// collapseSpaces folds the runs of spaces into one.
func collapseSpaces(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// limitStrings keeps the first n strings.
func limitStrings(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return values[:n]
}

// truncate keeps the first n characters of a message.
func truncate(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[:n]
}

// load reads the cache of the names.
func (n *GalaxyNamer) load() {
	if n.file == "" {
		return
	}
	raw, err := os.ReadFile(n.file)
	if err != nil {
		return
	}
	var parsed galaxyCacheFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return
	}
	if parsed.Version != galaxyCacheVersion {
		return
	}
	if parsed.Names != nil {
		n.cache.Names = parsed.Names
	}
	if parsed.Retry != nil {
		n.cache.Retry = parsed.Retry
	}
	n.cache.Calls = parsed.Calls
}

// persist writes the cache of the names.
func (n *GalaxyNamer) persist() {
	if n.file == "" {
		return
	}
	n.mu.Lock()
	raw, err := json.MarshalIndent(n.cache, "", "  ")
	n.mu.Unlock()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(n.file), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(n.file, raw, 0o600); err != nil {
		return
	}
}

// dayKey is the day a call belongs to.
//
// The timestamps of the calls are UTC, so the day must be counted in UTC as
// well: a node whose clock is in another zone would count the calls of the
// previous UTC day as today's, and the ceiling of the day would not bind
// between midnight and eight in the morning.
func dayKey(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
