// Package observer serves the read model of the storage core to clients that
// show what the fleet is doing: the git-ai view frontend and anything else
// that wants the same picture.
//
// The service owns every dependency the frontend must not have: the
// storage core's database, the operational state of the mirrors and the
// backups, the CI database and the model that names the galaxies. A client
// speaks HTTP and JSON to this package and never touches a database, a worker
// host or a model endpoint.
//
// The JSON shapes are the contract of the view frontend (git-ai-view); the
// field names are camelCase to match it.
package observer

// ActivityEvent is one activity record, in the order a client should play it.
type ActivityEvent struct {
	ID        string         `json:"id"`
	Cursor    int64          `json:"cursor"`
	At        string         `json:"at"`
	Kind      string         `json:"kind"`
	Severity  string         `json:"severity"`
	Repo      *string        `json:"repo"`
	RepoID    *int64         `json:"repoId"`
	Actor     *string        `json:"actor"`
	Node      *string        `json:"node"`
	Operation *string        `json:"operation"`
	Branch    *string        `json:"branch"`
	Commit    *string        `json:"commit"`
	Summary   string         `json:"summary"`
	Refs      []RefUpdate    `json:"refs"`
	Meta      map[string]any `json:"meta"`
}

// RefUpdate is one reference a changeset moved.
type RefUpdate struct {
	Name      string  `json:"name"`
	ShortName string  `json:"shortName"`
	Kind      string  `json:"kind"`
	OldOID    *string `json:"oldOid"`
	NewOID    *string `json:"newOid"`
}

// RepoRef is one reference of a repository, as the fleet shows it.
type RepoRef struct {
	Name      string `json:"name"`
	ShortName string `json:"shortName"`
	Target    string `json:"target"`
	Kind      string `json:"kind"`
	Seq       int64  `json:"seq"`
}

// RepoActor is one actor of a repository, with how much it changed.
type RepoActor struct {
	Name   string  `json:"name"`
	Count  int64   `json:"count"`
	LastAt *string `json:"lastAt"`
}

// RepoSummary is one repository of the fleet.
type RepoSummary struct {
	ID             int64       `json:"id"`
	Storage        string      `json:"storage"`
	Path           string      `json:"path"`
	ObjectFormat   string      `json:"objectFormat"`
	CreatedAt      string      `json:"createdAt"`
	HeadSeq        int64       `json:"headSeq"`
	LogSeq         int64       `json:"logSeq"`
	Changesets     int64       `json:"changesets"`
	SizeBytes      int64       `json:"sizeBytes"`
	LastActivityAt *string     `json:"lastActivityAt"`
	Actors         []RepoActor `json:"actors"`
	Branches       []RepoRef   `json:"branches"`
	Tags           []RepoRef   `json:"tags"`
	InternalRefs   []RepoRef   `json:"internalRefs"`
	Activity       []int       `json:"activity"`
	Health         string      `json:"health"`
}

// NodeSummary is one machine that applies the changesets of the fleet.
type NodeSummary struct {
	ID         string  `json:"id"`
	Repos      int     `json:"repos"`
	AppliedSeq int64   `json:"appliedSeq"`
	MaxHeadSeq int64   `json:"maxHeadSeq"`
	Lag        int64   `json:"lag"`
	UpdatedAt  *string `json:"updatedAt"`
	Status     string  `json:"status"`
}

// MirrorTarget is one mirror of the fleet, with the state of its last
// publication.
type MirrorTarget struct {
	ID          string   `json:"id"`
	Repo        string   `json:"repo"`
	URL         string   `json:"url"`
	Host        string   `json:"host"`
	Branches    []string `json:"branches"`
	Status      string   `json:"status"`
	LastPushAt  *string  `json:"lastPushAt"`
	LastEventID *string  `json:"lastEventId"`
	Detail      *string  `json:"detail"`
}

// UniverseStats are the counters the top bar shows.
type UniverseStats struct {
	Repos           int     `json:"repos"`
	Branches        int     `json:"branches"`
	Changesets      int64   `json:"changesets"`
	Actors          int     `json:"actors"`
	Nodes           int     `json:"nodes"`
	EventsPerMinute int     `json:"eventsPerMinute"`
	LandsPerMinute  float64 `json:"landsPerMinute"`
	MirrorTargets   int     `json:"mirrorTargets"`
	MirrorsHealthy  int     `json:"mirrorsHealthy"`
	LastEventAt     *string `json:"lastEventAt"`
	BytesWritten    int64   `json:"bytesWritten"`
}

// SourceInfo says where the picture comes from.
type SourceInfo struct {
	Mode           string   `json:"mode"`
	Label          string   `json:"label"`
	Database       *string  `json:"database"`
	Address        *string  `json:"address"`
	ActionsEnabled bool     `json:"actionsEnabled"`
	PollMs         int      `json:"pollMs"`
	StartedAt      string   `json:"startedAt"`
	Warnings       []string `json:"warnings"`
}

// Snapshot is the whole fleet in one document.
type Snapshot struct {
	Source      SourceInfo      `json:"source"`
	GeneratedAt string          `json:"generatedAt"`
	Cursor      int64           `json:"cursor"`
	Repos       []RepoSummary   `json:"repos"`
	Nodes       []NodeSummary   `json:"nodes"`
	Mirrors     []MirrorTarget  `json:"mirrors"`
	Events      []ActivityEvent `json:"events"`
	Stats       UniverseStats   `json:"stats"`
}

// HealthResponse is the answer of the health endpoint.
type HealthResponse struct {
	// OK is false while the storage core is unreachable, which means the data
	// on screen is stale.
	OK             bool         `json:"ok"`
	Mode           string       `json:"mode"`
	Database       *string      `json:"database"`
	ActionsEnabled bool         `json:"actionsEnabled"`
	UptimeSec      int64        `json:"uptimeSec"`
	Version        string       `json:"version"`
	Warnings       []string     `json:"warnings"`
	Galaxies       *GalaxyState `json:"galaxies,omitempty"`
	// System holds the deeper probes of the health page.
	System *SystemProbe `json:"system,omitempty"`
}

// GalaxyState is the state of the galaxy namer.
type GalaxyState struct {
	Configured bool    `json:"configured"`
	Names      int     `json:"names"`
	CallsToday int     `json:"callsToday"`
	DailyLimit int     `json:"dailyLimit"`
	LastCallAt *string `json:"lastCallAt"`
	LastError  *string `json:"lastError"`
}
