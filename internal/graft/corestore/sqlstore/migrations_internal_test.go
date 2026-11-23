package sqlstore

import (
	"testing"

	migrate "github.com/rubenv/sql-migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShippedMigrationIDsAreNotRenamed pins the ids that a deployed database
// already records. A migration that has run somewhere is history: renaming it
// makes refuseUnknownMigrations reject that database ("the database was
// migrated by a newer version of the storage core") and the node refuses to
// start.
//
// 20261117000000_graft_mirror_state shipped in v0.7.0 and is recorded by the
// production and staging databases. A merge that added the archive migration
// moved it to 20261117100000, and both lanes then refused to start on main.
func TestShippedMigrationIDsAreNotRenamed(t *testing.T) {
	sets := map[string][]*migrate.Migration{
		"sqlite":   sqliteMigrations,
		"postgres": postgresMigrations,
	}
	for name, migrations := range sets {
		t.Run(name, func(t *testing.T) {
			ids := make(map[string]struct{}, len(migrations))
			for _, m := range migrations {
				_, duplicate := ids[m.Id]
				require.Falsef(t, duplicate, "migration id %q appears twice", m.Id)
				ids[m.Id] = struct{}{}
			}
			assert.Contains(t, ids, "20261117000000_graft_mirror_state",
				"the mirror state migration shipped under this id; renaming it locks every deployed database out")
			assert.NotContains(t, ids, "20261117100000_graft_mirror_state",
				"this id never shipped; a database records 20261117000000_graft_mirror_state")
		})
	}
}
