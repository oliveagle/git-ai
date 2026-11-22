package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/sdk"
)

// storageSynopsis is the command line of graft storage.
const storageSynopsis = "usage: graft storage import|state|verify|checkpoint|archive|unarchive|archived|cleanup-start|purge|gc [-repo R] [--connectivity] [--dry-run] [--removed] [--json]"

// storageUsage lists the subcommands of graft storage.
const storageUsage = "storage requires a subcommand: import, state, verify, checkpoint, archive, unarchive, archived, cleanup-start, purge or gc (" + storageSynopsis + ")"

// storageFlags holds the flags of graft storage. Each subcommand reads only
// some of them, as its help text says, and rejects the others (checkUsed).
type storageFlags struct {
	repo         string
	connectivity bool
	dryRun       bool
	removed      bool
}

// storageCommand returns the storage command, the operator's view of the
// storage core (import, state and verify of a repository).
func storageCommand() command {
	f := &storageFlags{}
	return command{
		bind: func(fs *flag.FlagSet, o *options) {
			fs.Usage = func() {
				fmt.Fprintf(fs.Output(), "%s\n\nflags:\n", storageSynopsis)
				fs.PrintDefaults()
			}
			fs.StringVar(&f.repo, "repo", "", "repository path, used without a workspace together with -address and -storage")
			fs.BoolVar(&f.connectivity, "connectivity", false, "verify: also check the connectivity of the local repository (git fsck --connectivity-only) and that the object storage holds every object of the database references")
			fs.BoolVar(&f.dryRun, "dry-run", false, "gc: report what would be deleted, delete nothing")
			fs.BoolVar(&f.removed, "removed", false, "gc: collect the removed repositories of the storage (-storage) instead of one repository; takes no -repo")
			fs.BoolVar(&o.jsonOut, "json", false, "print the result as one JSON object")
		},
		run: func(ctx context.Context, out io.Writer, o *options, args []string) error {
			return cmdStorage(ctx, out, o, f, args)
		},
	}
}

// cmdStorage dispatches a storage subcommand. Arguments are checked before the
// server is contacted.
func cmdStorage(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) == 0 {
		return usagef("%s", storageUsage)
	}
	sub, rest := args[0], args[1:]
	if used, known := storageSubcommandFlags[sub]; known {
		if err := f.checkUsed(sub, used); err != nil {
			return err
		}
	}
	if o.adminToken != "" && !slices.Contains([]string{"cleanup-start", "purge"}, sub) {
		return usagef("storage %s does not take --admin-token: -admin-token is only for cleanup-start and purge", sub)
	}
	switch sub {
	case "import":
		return storageImport(ctx, out, o, f, rest)
	case "state":
		return storageState(ctx, out, o, f, rest)
	case "verify":
		return storageVerify(ctx, out, o, f, rest)
	case "checkpoint":
		return storageCheckpoint(ctx, out, o, f, rest)
	case "archive":
		return storageArchive(ctx, out, o, f, rest)
	case "unarchive":
		return storageUnarchive(ctx, out, o, f, rest)
	case "archived":
		return storageArchived(ctx, out, o, f, rest)
	case "cleanup-start":
		return storageCleanupStart(ctx, out, o, f, rest)
	case "purge":
		return storagePurge(ctx, out, o, f, rest)
	case "gc":
		return storageGC(ctx, out, o, f, rest)
	}
	return usagef("unknown storage subcommand %q: %s", sub, storageUsage)
}

// storageSubcommandFlags are the subcommand-specific flags each subcommand
// reads.
var storageSubcommandFlags = map[string][]string{
	"import":        nil,
	"state":         nil,
	"verify":        {"connectivity"},
	"checkpoint":    nil,
	"archive":       nil,
	"unarchive":     nil,
	"archived":      nil,
	"cleanup-start": nil,
	"purge":         nil,
	"gc":            {"dry-run", "removed"},
}

// checkUsed rejects a subcommand-specific flag that the subcommand sub does not
// read: a flag that does nothing must not look accepted (checkpoint --dry-run
// would create a checkpoint).
func (f *storageFlags) checkUsed(sub string, used []string) error {
	for _, flag := range []struct {
		name string
		set  bool
	}{
		{"connectivity", f.connectivity},
		{"dry-run", f.dryRun},
		{"removed", f.removed},
	} {
		if flag.set && !slices.Contains(used, flag.name) {
			return usagef("storage %s does not take --%s: %s", sub, flag.name, storageSynopsis)
		}
	}
	return nil
}

func storageCheckpoint(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage checkpoint takes flags only: checkpoint [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageCheckpoint(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	switch {
	case res.Sequence == 0:
		fmt.Fprintln(out, "no checkpoint: the repository has no object to consolidate")
	case res.Created:
		fmt.Fprintf(out, "checkpoint created at changeset %d: %d references, pack %d bytes\n", res.Sequence, res.References, res.PackBytes)
	default:
		fmt.Fprintf(out, "checkpoint at changeset %d exists already: %d references, pack %d bytes\n", res.Sequence, res.References, res.PackBytes)
	}
	return nil
}

func storageGC(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage gc takes flags only: gc [-repo R] [--dry-run] [--json], or gc --removed [-storage S] [--dry-run] [--json]")
	}
	if f.removed {
		if f.repo != "" {
			return usagef("storage gc --removed collects the removed repositories of a storage and takes no -repo")
		}
		return storageGCRemoved(ctx, out, o, f)
	}

	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageGC(ctx, f.dryRun)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	verb := "deleted"
	if res.DryRun {
		verb = "would delete"
	}
	fmt.Fprintf(out, "%s %d objects (%d bytes): %d covered by the checkpoint at changeset %d, %d superseded checkpoints, %d orphans\n",
		verb, res.PacksDeleted, res.BytesReclaimed, res.CoveredDeleted, res.Boundary, res.CheckpointsDropped, res.OrphansDeleted)
	fmt.Fprintf(out, "orphans observed now %d, still in their grace period %d\n", res.OrphansObserved, res.OrphansInGrace)
	return nil
}

func storageGCRemoved(ctx context.Context, out io.Writer, o *options, f *storageFlags) error {
	client, err := openStorageClient(ctx, o)
	if err != nil {
		return err
	}
	res, err := client.StorageGCRemoved(ctx, "", f.dryRun)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	verb := "deleted"
	if res.DryRun {
		verb = "would delete"
	}
	fmt.Fprintf(out, "%s %d objects (%d bytes) of %d removed repositories of storage %s\n",
		verb, res.PacksDeleted, res.BytesReclaimed, res.Repositories, displayText(client.Storage()))
	if res.LeaksFound > 0 {
		recorded := "recorded"
		if res.DryRun {
			recorded = "would record"
		}
		fmt.Fprintf(out, "found %d repository IDs with objects and no row or tombstone; %s %d tombstones (their objects are deleted after gc_grace)\n",
			res.LeaksFound, recorded, res.LeaksTombstoned)
	}
	if res.IDsAheadOfAllocator > 0 {
		fmt.Fprintf(out, "warning: %d repository IDs above the database's allocator have objects in the bucket (a database restored from an older backup?); they are left alone\n",
			res.IDsAheadOfAllocator)
	}
	return nil
}

// openStorageClient returns the client of a storage-wide command: the server
// and the storage of the workspace, or without a workspace -address and
// -storage.
func openStorageClient(ctx context.Context, o *options) (*sdk.Client, error) {
	var t target
	if root, err := workspaceRoot(o); err == nil {
		if meta, err := workspace.ReadMeta(root); err == nil {
			t = target{Address: pick(o.address, meta.Address), Token: o.token, AgentKey: o.agentKey, Storage: pick(o.storage, meta.Storage), Repo: meta.Repo, User: o.user}
		}
	}
	if t.Address == "" {
		if o.address == "" {
			return nil, usagef("storage gc --removed needs a workspace or -address")
		}
		t = target{Address: o.address, Token: o.token, AgentKey: o.agentKey, Storage: o.storage, User: o.user}
	}

	// A storage-wide command names no repository, which the workspace backend
	// requires: the client is dialed with the same options.
	client, err := sdk.Dial(ctx, t.Address, dialOptions(t)...)
	if err != nil {
		return nil, err
	}
	registerCloser(ctx, client)
	return client, nil
}

func storageImport(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage import takes flags only: import [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageImport(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	if res.References == 0 {
		// A history import: a first import always records HEAD.
		fmt.Fprintf(out, "already imported at changeset %d: imported %d changelog entries and %d reviews written since\n",
			res.Sequence, res.ChangelogEntries, res.Reviews)
		return nil
	}
	fmt.Fprintf(out, "imported repository: changeset %d, %d references, %d changelog entries, %d reviews, pack %d bytes\n",
		res.Sequence, res.References, res.ChangelogEntries, res.Reviews, res.PackBytes)
	return nil
}

func storageState(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage state takes flags only: state [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	st, err := repo.StorageState(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, st)
	}
	// The node name comes from the configuration of the server, so it is shown escaped.
	fmt.Fprintf(out, "mode\t%s\n", displayText(st.Mode))
	fmt.Fprintf(out, "tracked\t%t\n", st.Tracked)
	fmt.Fprintf(out, "repository_id\t%d\n", st.RepositoryID)
	fmt.Fprintf(out, "head_sequence\t%d\n", st.HeadSequence)
	fmt.Fprintf(out, "applied_sequence\t%d\n", st.AppliedSequence)
	fmt.Fprintf(out, "node_id\t%s\n", displayText(st.NodeID))
	fmt.Fprintf(out, "archived\t%t\n", st.Archived)
	if st.Archived {
		if st.ReadClosed {
			fmt.Fprintf(out, "read_closed_at\t%s\n", st.ReadClosedAt.UTC().Format(time.RFC3339))
			fmt.Fprintf(out, "purge_after\t%s\n", st.PurgeAfter.UTC().Format(time.RFC3339))
		} else {
			fmt.Fprintln(out, "reads\topen")
		}
	}
	return nil
}

func storageVerify(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage verify takes flags only: verify [-repo R] [--connectivity] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageVerify(ctx, f.connectivity)
	if err != nil {
		return mapError(err)
	}

	if o.jsonOut {
		if err := writeStorageJSON(out, storageVerifyView(res)); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "head sequence %d\n", res.HeadSequence)
		// Reference names come from the database and the repository: displayText
		// keeps control characters in them from forging lines of the output.
		for _, d := range res.Divergences {
			fmt.Fprintf(out, "divergence\t%s\tlocal=%s\tdatabase=%s\n",
				displayText(d.Reference), storageTargetText(d.LocalTarget), storageTargetText(d.DatabaseTarget))
		}
		if f.connectivity {
			if res.ConnectivityOK {
				fmt.Fprintln(out, "connectivity ok")
			} else {
				fmt.Fprintf(out, "connectivity failed: %s\n", displayText(res.ConnectivityError))
			}
		}
		if len(res.Divergences) == 0 && (!f.connectivity || res.ConnectivityOK) {
			fmt.Fprintln(out, "consistent")
		}
	}

	if len(res.Divergences) > 0 {
		return fmt.Errorf("storage verify: %d reference(s) diverge from the database", len(res.Divergences))
	}
	if f.connectivity && !res.ConnectivityOK {
		return errors.New("storage verify: connectivity check failed")
	}
	return nil
}

// storageArchive freezes the repository: it is kept, frozen and readable, and
// no update may change it any more. The server never removes a repository, so
// this is the way to retire one.
func storageArchive(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage archive takes flags only: archive [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageArchive(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	fmt.Fprintf(out, "archived by %s at %s: writes are refused and reads remain open until cleanup starts\n",
		displayText(res.ArchivedBy), res.ArchivedAt.UTC().Format(time.RFC3339))
	return nil
}

// storageCleanupStart closes the reads of an archived repository and starts
// its observation period. It is the admin-only first step of deletion.
func storageCleanupStart(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage cleanup-start takes flags only: cleanup-start [-repo R] [--json]")
	}
	repo, err := openStorageAdmin(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StorageStartCleanup(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	fmt.Fprintf(out, "reads closed by %s at %s: it may be purged from %s\n",
		displayText(res.ReadClosedBy), res.ReadClosedAt.UTC().Format(time.RFC3339), res.PurgeAfter.UTC().Format(time.RFC3339))
	return nil
}

// storageUnarchive takes an archived repository out of the archive: it is
// writable again.
func storageUnarchive(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage unarchive takes flags only: unarchive [-repo R] [--json]")
	}
	repo, err := openStorage(ctx, o, f)
	if err != nil {
		return err
	}
	if err := repo.StorageUnarchive(ctx); err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, struct{}{})
	}
	fmt.Fprintln(out, "unarchived: the repository is writable again")
	return nil
}

// storageArchived lists the archived repositories of the storage: the ones
// that are frozen and waiting for the super admin to purge them.
func storageArchived(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage archived takes flags only: archived [-storage S] [--json]")
	}
	if f.repo != "" {
		return usagef("storage archived lists the archived repositories of a storage and takes no -repo")
	}
	client, err := openStorageClient(ctx, o)
	if err != nil {
		return err
	}
	repos, err := client.StorageArchived(ctx, "")
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, repos)
	}
	for _, r := range repos {
		state := "reads open"
		if !r.ReadClosedAt.IsZero() {
			state = "read closed; purgeable from " + r.PurgeAfter.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "%s\tarchived at %s by %s\t%s\n",
			displayText(r.StorageName+":"+r.RelativePath),
			r.ArchivedAt.UTC().Format(time.RFC3339), displayText(r.ArchivedBy), state)
	}
	return nil
}

// storagePurge deletes an archived repository whose cleanup observation has
// passed. It needs the explicit super-admin token: it is the one operation
// that destroys a repository.
func storagePurge(ctx context.Context, out io.Writer, o *options, f *storageFlags, args []string) error {
	if len(args) != 0 {
		return usagef("storage purge takes flags only: purge [-repo R] [--json]")
	}
	repo, err := openStorageAdmin(ctx, o, f)
	if err != nil {
		return err
	}
	res, err := repo.StoragePurge(ctx)
	if err != nil {
		return mapError(err)
	}
	if o.jsonOut {
		return writeStorageJSON(out, res)
	}
	fmt.Fprintf(out, "purged: repository ID %d; the garbage collector deletes its objects after object_expiration\n", res.RepositoryID)
	return nil
}

// openStorageAdmin returns a repository handle that authenticates only with the
// explicit super-admin token. An agent key must never sign cleanup or purge.
func openStorageAdmin(ctx context.Context, o *options, f *storageFlags) (*sdk.Repo, error) {
	if o.adminToken == "" {
		return nil, usagef("storage cleanup and purge need -admin-token or GRAFT_ADMIN_TOKEN")
	}
	t, _, err := reviewTarget(o, &reviewFlags{repo: f.repo})
	if err != nil {
		return nil, err
	}
	t.AdminToken = o.adminToken
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, errors.New("storage: the backend cannot reach the storage core")
	}
	return sb.repo, nil
}

// openStorage returns the repository handle of a storage command. The server
// comes from the workspace, or without a workspace from -address, -storage and
// -repo, exactly as for graft review.
func openStorage(ctx context.Context, o *options, f *storageFlags) (*sdk.Repo, error) {
	t, _, err := reviewTarget(o, &reviewFlags{repo: f.repo})
	if err != nil {
		return nil, err
	}
	backend, err := backendFactory(ctx, t)
	if err != nil {
		return nil, err
	}
	sb, ok := backend.(*sdkBackend)
	if !ok {
		return nil, errors.New("storage: the backend cannot reach the storage core")
	}
	return sb.repo, nil
}

// storageTargetText shows a reference value, with the empty value as absent.
func storageTargetText(t string) string {
	if t == "" {
		return "absent"
	}
	return displayText(t)
}

// storageDivergenceJSON is a divergence in the JSON output of verify.
type storageDivergenceJSON struct {
	sdk.StorageDivergence
	// ReferenceBytes is the exact reference name (base64) when it is not valid
	// UTF-8: JSON strings cannot carry such bytes, so "reference" holds them
	// replaced by U+FFFD.
	ReferenceBytes []byte `json:"reference_bytes,omitempty"`
}

// storageVerifyJSON is the JSON output of verify: the SDK result, with the
// divergences extended by storageDivergenceJSON.
type storageVerifyJSON struct {
	*sdk.StorageVerify
	Divergences []storageDivergenceJSON `json:"divergences"`
}

// storageVerifyView returns the JSON output of a verify result.
func storageVerifyView(res *sdk.StorageVerify) storageVerifyJSON {
	view := storageVerifyJSON{StorageVerify: res}
	if res.Divergences != nil {
		view.Divergences = make([]storageDivergenceJSON, 0, len(res.Divergences))
	}
	for _, d := range res.Divergences {
		entry := storageDivergenceJSON{StorageDivergence: d}
		if !utf8.ValidString(d.Reference) {
			entry.ReferenceBytes = []byte(d.Reference)
		}
		view.Divergences = append(view.Divergences, entry)
	}
	return view
}

// writeStorageJSON prints v as one line of JSON.
func writeStorageJSON(out io.Writer, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", line)
	return err
}
