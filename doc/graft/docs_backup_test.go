package graft

// Black-box spec test for the backup policy (doc/graft/BACKUP.md).
//
// The policy is only a policy when something enforces it, so the document has
// to name the rule, say what makes a backup count, and say what the gate does
// when a repository has no origin.

import (
	"strings"
	"testing"
)

// Spec: every repository of the storage core must have a backup origin, and a
// backup counts only when the origin carries the same commit ids.
func TestSpecBackupPolicy(t *testing.T) {
	const file = "BACKUP.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text,
		"backup origin",
		"commit id",
		"backup-origins.json",
		"backup-origin-gate.sh",
		"mirror-push.sh",
	)

	if !specLine(text, `no exception`) {
		t.Errorf("%s: the rule must say that it has no exception", file)
	}
	if !specLine(text, `without an origin is a failure`) {
		t.Errorf("%s: a repository without an origin must be stated as a failure", file)
	}
	if !specLine(text, `A push is not a backup`) {
		t.Errorf("%s: the document must say that a push is not a backup", file)
	}
}

// Spec: the gate is a schedule, not a wish, and its exit status is the report.
func TestSpecBackupGateIsScheduled(t *testing.T) {
	const file = "BACKUP.md"
	text := specDoc(t, file)

	if !specLine(text, `cron`) {
		t.Errorf("%s: the gate must be named as the job of a schedule", file)
	}
	if !specLine(text, `--sync`) {
		t.Errorf("%s: the document must describe the publish-then-verify run", file)
	}
}

// Spec: the recovery path is named, because the origin exists for it. The
// review state does not travel with the mirror.
func TestSpecBackupRecovery(t *testing.T) {
	const file = "BACKUP.md"
	text := specDoc(t, file)

	for _, want := range []string{"graft storage import", "refs/graft/"} {
		if !strings.Contains(text, want) {
			t.Errorf("%s must mention %q", file, want)
		}
	}
}

// Spec: the checkouts on the workstation obey the same rule, with its own
// gate, and the unit of the check is the commit and not the name of a branch.
func TestSpecBackupOfTheCheckouts(t *testing.T) {
	const file = "BACKUP.md"
	text := specDoc(t, file)

	for _, want := range []string{
		"local-backup-gate.sh",
		"backup-origins.local.json",
		"the unit of the check is the commit",
		"uncommitted changes are counted",
	} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("%s must mention %q", file, want)
		}
	}
	if !specLine(text, `not forced`) {
		t.Errorf("%s: the document must say that a diverged branch is not forced", file)
	}
}
