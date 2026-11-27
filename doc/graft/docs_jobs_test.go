package graft

// Black-box spec test for the job layer (doc/graft/JOBS.md).
//
// Every periodic job of a deployment is a workflow of a Temporal server. The
// document therefore has to name every job, the two halves of the layer, the
// time zone the calendar runs in, and the way a node is installed — because a
// job that is not on the calendar is a job that runs nowhere.

import (
	"testing"
)

// Spec: the document names every job of a deployment, by the identifier its
// schedule carries. A job that the document forgets is a job that a node
// cannot run.
func TestSpecEveryJobIsNamed(t *testing.T) {
	const file = "JOBS.md"
	text := specDoc(t, file)

	specRequireContains(t, file, text,
		"graft-mirror-sweep",
		"graft-mirror-tier",
		"graft-remote-import",
		"graft-backup-gate",
		"graft-database-dump",
		"graft-database-dump-ci",
		"graft-local-backup-gate",
		"graft-local-backup-full",
		"graft-local-backup-incremental",
	)
}

// Spec: the two halves of the layer are named, and the calendar is on the
// server of Temporal in the namespace of the deployment.
func TestSpecTheCalendarLivesOnTheServer(t *testing.T) {
	const file = "JOBS.md"
	text := specDoc(t, file)

	for _, want := range []string{"Temporal", "namespace `graft`", "workflow"} {
		if !specLine(text, want) {
			t.Errorf("%s must name %q, but it does not", file, want)
		}
	}
	if !specLine(text, `Asia/Shanghai`) {
		t.Errorf("%s: the calendar must name the time zone of the node", file)
	}
	if !specLine(text, `graft-jobs-workstation`) {
		t.Errorf("%s: the worker of the workstation must have its own queue", file)
	}
}

// Spec: a node is installed with its calendar. The install registers the
// schedules, because a worker without a calendar runs nothing.
func TestSpecTheInstallRegistersTheSchedules(t *testing.T) {
	const file = "JOBS.md"
	text := specDoc(t, file)

	if !specLine(text, `install-worker\.sh`) {
		t.Errorf("%s: the document must name the script that installs the worker", file)
	}
	if !specLine(text, `graft-worker register`) {
		t.Errorf("%s: the document must name the command that puts the schedules on the server", file)
	}
	if !specLine(text, `commented out`) {
		t.Errorf("%s: the document must say how the old way is put back", file)
	}
}

// Spec: the scope of the layer is a decision and not an accident. The jobs of
// another project are left alone, and the document says so.
func TestSpecTheScopeOfTheLayerIsWrittenDown(t *testing.T) {
	const file = "JOBS.md"
	text := specDoc(t, file)

	if !specLine(text, `another project`) {
		t.Errorf("%s: the document must say that the jobs of another project are left alone", file)
	}
	if !specLine(text, `agents-sync\.timer`) {
		t.Errorf("%s: the document must name the jobs that are not of the deployment", file)
	}
}
