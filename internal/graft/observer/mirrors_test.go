package observer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The mirror page joins three authorities: the queue of the operator holds
// the plan, the graft server holds the measured state and the backup gate
// holds the verdict about the origins. The tests below keep that join honest.

func TestComposeMirrorsJoinsTheQueueTheServerAndTheGate(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	enabled := true
	targets := []queueTarget{
		{
			Name:       "github",
			Repository: "demo",
			URL:        "ssh://git@github.com/oliveagle/demo.git",
			Branches:   []string{"main"},
			Interval:   300000,
			Enabled:    &enabled,
			NextRun:    "2026-11-26T12:05:00Z",
		},
	}
	tasks := []queueTask{
		{
			ID:         "task-1",
			Target:     "github",
			Repository: "demo",
			Status:     "ok",
			CreatedAt:  "2026-11-26T11:59:00Z",
			StartedAt:  "2026-11-26T11:59:01Z",
			FinishedAt: "2026-11-26T11:59:03Z",
		},
	}
	lag := map[string]mirrorLag{
		"github": {
			Status:      "ok",
			Lag:         int64Ptr(0),
			PublishedAt: stringPtr("2026-11-26T11:59:04Z"),
			Published:   map[string]string{"main": "3e306adec079d7fad84a8d71f32df0b914e956aa"},
		},
	}
	policy := &MirrorPolicy{Required: true, WarnLag: 5, DenyLag: 60}
	gate := &BackupGateSummary{
		Result:       "FAIL",
		Repositories: 1,
		InSync:       1,
		Drifted:      0,
		Origins: []BackupOrigin{
			{Repository: "demo", Origin: "github-demo", Status: BackupInSync},
		},
	}
	disk := &HostDisk{Host: "node", Role: "worker", Mount: "/", TotalBytes: 100, FreeBytes: 50, UsedPct: 50}
	mtime := "2026-11-26T11:58:00Z"

	response := composeMirrors(Config{}, targets, tasks, lag, policy, gate, disk, &mtime, nil, nil, now)

	if len(response.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(response.Targets))
	}
	target := response.Targets[0]
	if target.Name != "github" || target.Repository != "demo" {
		t.Fatalf("target = %+v, want the github target of demo", target)
	}
	if !target.Enabled {
		t.Fatal("the target is enabled, want enabled")
	}
	if target.Lag == nil || *target.Lag != 0 {
		t.Fatalf("lag = %v, want 0: a mirror that is in sync says zero, not nothing", target.Lag)
	}
	if target.PublishedAt == nil || *target.PublishedAt != "2026-11-26T11:59:04Z" {
		t.Fatalf("publishedAt = %v, want the stamp of the server", target.PublishedAt)
	}
	if target.LastStatus != "ok" {
		t.Fatalf("lastStatus = %q, want ok", target.LastStatus)
	}
	if response.Summary.Targets != 1 || response.Summary.OK != 1 {
		t.Fatalf("summary = %+v, want one target and one ok", response.Summary)
	}
	if response.Backup == nil || response.Backup.Result != "FAIL" {
		t.Fatalf("backup = %+v, want the verdict of the gate", response.Backup)
	}
	if len(response.Runs) == 0 {
		t.Fatal("runs are empty, want the task of the queue")
	}
	if response.Runs[0].DurationMs == nil || *response.Runs[0].DurationMs != 2000 {
		t.Fatalf("duration = %v, want 2000ms", response.Runs[0].DurationMs)
	}
}

func TestSummariseMirrorsCountsTheFabric(t *testing.T) {
	lag := int64Ptr(3)
	targets := []MirrorTargetIntel{
		{Name: "a", Enabled: true, Lag: lag, SyncStatus: stringPtr("ok"), LastStatus: "ok"},
		{Name: "b", Enabled: true, Lag: lag, SyncStatus: stringPtr("failed"), LastStatus: "failed"},
		{Name: "c", Enabled: false, SyncStatus: stringPtr("ok"), LastStatus: "ok"},
	}
	summary := summariseMirrors(targets, nil)

	if summary.Targets != 3 || summary.Enabled != 2 || summary.Disabled != 1 {
		t.Fatalf("summary = %+v, want 3 targets, 2 enabled and 1 disabled", summary)
	}
	if summary.OK != 2 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want two ok and one failed", summary)
	}
	if summary.Stale != 2 {
		t.Fatalf("summary = %+v, want the two targets without a recent success to be stale", summary)
	}
	if summary.MaxLag == nil || *summary.MaxLag != 3 {
		t.Fatalf("maxLag = %v, want 3", summary.MaxLag)
	}
	if summary.AvgLag == nil || *summary.AvgLag != 3 {
		t.Fatalf("avgLag = %v, want 3", summary.AvgLag)
	}
}

func TestClassifyMirrorReadsTheSyncStatus(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"ok", "ok"},
		{"failed", "failed"},
		{"pending", "idle"},
	}
	for _, test := range cases {
		got := classifyMirror(optional(test.status), "ok")
		if got != test.want {
			t.Fatalf("classifyMirror(%q) = %q, want %q", test.status, got, test.want)
		}
	}
	if got := classifyMirror(nil, "ok"); got != "ok" {
		t.Fatalf("classifyMirror(nil) = %q, want the last status to answer", got)
	}
	if got := classifyMirror(nil, ""); got != "idle" {
		t.Fatalf("classifyMirror(nil, empty) = %q, want idle", got)
	}
}

func TestGateStatusMapsTheWordsOfTheGate(t *testing.T) {
	cases := map[string]BackupOriginStatus{
		"in_sync":       BackupInSync,
		"OK":            BackupInSync,
		"matches":       BackupInSync,
		"out_of_sync":   BackupDrifted,
		"drifted":       BackupDrifted,
		"missing":       BackupMissing,
		"not_found":     BackupMissing,
		"":              BackupFailed,
		"somethingelse": BackupFailed,
	}
	for raw, want := range cases {
		if got := gateStatus(raw); got != want {
			t.Fatalf("gateStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestReadGateReportReadsTheNewestReport(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "backup-origins-20260101T000000Z.json")
	newer := filepath.Join(dir, "backup-origins-20260101T010000Z.json")
	write(t, older, `{"checked_at":"2026-01-01T00:00:00Z","result":"PASS","repositories":[{"repository":"a","origin":"x","status":"in_sync"}]}`)
	write(t, newer, `{"checked_at":"2026-01-01T01:00:00Z","result":"FAIL","repositories":[{"repository":"b","origin":"x","status":"in_sync","detail":"2 refs match"},{"repository":"b","origin":"y","status":"out_of_sync"},{"repository":"c","origin":"z","status":"missing"},{"repository":"d","origin":"w","status":"broken"}]}`)
	if err := os.Chtimes(older, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(newer, time.Now(), time.Now()); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	gate := readGateReport(dir)
	if gate == nil {
		t.Fatal("the gate is nil, want the newest report")
	}
	if gate.Result != "FAIL" {
		t.Fatalf("result = %q, want FAIL: the newest report decides", gate.Result)
	}
	if gate.Repositories != 3 {
		t.Fatalf("repositories = %d, want 3 distinct repositories", gate.Repositories)
	}
	if gate.InSync != 1 || gate.Drifted != 1 || gate.Missing != 1 || gate.Failed != 1 {
		t.Fatalf("gate = %+v, want one of every verdict", gate)
	}
	if gate.CheckedAt == nil || *gate.CheckedAt != "2026-01-01T01:00:00.000Z" {
		t.Fatalf("checkedAt = %v, want the stamp of the newest report", gate.CheckedAt)
	}
}

func TestReadGateReportOfAMissingDirectoryIsNotAnError(t *testing.T) {
	if gate := readGateReport(filepath.Join(t.TempDir(), "nowhere")); gate != nil {
		t.Fatalf("gate = %+v, want nil: a missing report is not a failure", gate)
	}
}

func TestMedianInt64AveragesTheMiddleOfAnEvenList(t *testing.T) {
	if got := medianInt64([]int64{1, 2, 3, 4}); got == nil || *got != 3 {
		t.Fatalf("median = %v, want 3: the middle of an even list is rounded", got)
	}
	if got := medianInt64([]int64{1, 2, 3}); got == nil || *got != 2 {
		t.Fatalf("median = %v, want 2", got)
	}
	if got := medianInt64(nil); got != nil {
		t.Fatalf("median = %v, want nil", got)
	}
}

func TestRatioIsNilWhenThereIsNothingToDivide(t *testing.T) {
	if got := ratio(1, 0); got != nil {
		t.Fatalf("ratio = %v, want nil", got)
	}
	if got := ratio(1, 4); got == nil || *got != 0.25 {
		t.Fatalf("ratio = %v, want 0.25", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func int64Ptr(value int64) *int64 { return &value }
