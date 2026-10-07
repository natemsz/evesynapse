package app

import (
	"context"
	"database/sql"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// snapshotMeta is a stored character snapshot without its payload:
// which kind it is and how fresh.
//
// The payloads are by far the bulk of the snapshot table — a whole
// asset list, a mailbox, a wallet journal — and several readers want
// only the bookkeeping beside them: the worker ordering characters
// by what is most overdue (every character, every minute), the
// character sheet asking whether anything has arrived yet, and the
// Characters and Admin pages counting what is stored. Those read
// this instead of the full rows.
type snapshotMeta struct {
	Kind        string
	FetchedAt   string
	CachedUntil sql.NullString
}

// fresh reports whether the snapshot is still inside its cache
// window, by the same rule as esi.SnapshotFresh.
func (m snapshotMeta) fresh() bool {
	return esi.SnapshotFresh(db.CharacterSnapshot{CachedUntil: m.CachedUntil})
}

// listSnapshotMeta lists a character's stored snapshots, payloads
// left in the database, ordered by kind like ListSnapshotsByCharacter.
func (app *Application) listSnapshotMeta(ctx context.Context, characterID int64) ([]snapshotMeta, error) {
	rows, err := app.db.QueryContext(ctx,
		`SELECT kind, fetched_at, cached_until FROM character_snapshots WHERE character_id = $1 ORDER BY kind`,
		characterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []snapshotMeta
	for rows.Next() {
		var m snapshotMeta
		if err := rows.Scan(&m.Kind, &m.FetchedAt, &m.CachedUntil); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}
