package database

import (
	"encoding/json"
	"fmt"

	"github.com/TheQueenIsDead/budge/pkg/database/buckets"
	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

// migration changes stored data from one shape to the next. Each runs once,
// in its own transaction, in the order listed. Migrations are frozen: one
// that has shipped is never edited, since instances that ran it would not
// run it again. A fix goes in as a new migration.
type migration struct {
	name string
	run  func(tx *bolt.Tx) error
}

var migrations = []migration{
	{"spend groups onto category rules", migrateSpendGroups},
}

// schemaVersionKey holds how many migrations have run, in the settings bucket.
var schemaVersionKey = []byte("schema_version")

func schemaVersion(tx *bolt.Tx) (int, error) {
	raw := tx.Bucket(buckets.SettingsBucket).Get(schemaVersionKey)
	if raw == nil {
		return 0, nil
	}
	var version int
	return version, json.Unmarshal(raw, &version)
}

// migrate brings the database up to date. Before the first pending migration
// it copies the whole file beside itself, so a migration that goes wrong can
// be undone by hand by swapping the copy back in.
func migrate(db *bolt.DB, path string) error {
	var version int
	if err := db.View(func(tx *bolt.Tx) (err error) {
		version, err = schemaVersion(tx)
		return err
	}); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= len(migrations) {
		return nil
	}

	backup := fmt.Sprintf("%s.pre-migration-%d", path, version+1)
	if err := db.View(func(tx *bolt.Tx) error { return tx.CopyFile(backup, 0600) }); err != nil {
		return fmt.Errorf("back up before migrating: %w", err)
	}

	for i := version; i < len(migrations); i++ {
		m := migrations[i]
		err := db.Update(func(tx *bolt.Tx) error {
			if err := m.run(tx); err != nil {
				return err
			}
			raw, err := json.Marshal(i + 1)
			if err != nil {
				return err
			}
			return tx.Bucket(buckets.SettingsBucket).Put(schemaVersionKey, raw)
		})
		if err != nil {
			return fmt.Errorf("migration %d (%s): %w; the database is unchanged by it, and %s holds it as it was before migrating", i+1, m.name, err, backup)
		}
		log.Infof("database: ran migration %d, %s", i+1, m.name)
	}
	return nil
}

// getIn reads one document inside a transaction. found is false when the key
// was never written.
func getIn[T Storable](tx *bolt.Tx, key []byte) (value T, found bool, err error) {
	raw := tx.Bucket(value.Bucket()).Get(key)
	if raw == nil {
		return value, false, nil
	}
	return value, true, json.Unmarshal(raw, &value)
}
