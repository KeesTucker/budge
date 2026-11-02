package database

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TheQueenIsDead/budge/pkg/database/buckets"
	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// legacyDatabase writes a database as the fork left it before spend groups
// moved onto rules: the owner edited groceries, made a rent group, put one
// transaction in a group by hand and left another out of the insights.
func legacyDatabase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := bolt.Open(filepath.Join(dir, "budge.bolt.db"), 0600, nil)
	require.NoError(t, err)
	put := func(bucket, key []byte, value any) {
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, db.Update(func(tx *bolt.Tx) error {
			b, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return err
			}
			return b.Put(key, raw)
		}))
	}
	put(buckets.SettingsBucket, models.SpendRules{}.Key(), models.SpendRules{
		Groups: map[string]models.SpendRule{"groceries": {Keywords: []string{"veges"}, Merchants: []string{"Ooooby"}}},
		Custom: []models.CustomSpendGroup{{Key: "custom-rent", Label: "Rent", SpendRule: models.SpendRule{Keywords: []string{"rent"}}}},
	})
	put(buckets.SettingsBucket, models.InsightExclusions{}.Key(), models.InsightExclusions{TransactionIDs: []string{"trans_bond"}})
	put(buckets.SettingsBucket, models.InsightAssignments{}.Key(), models.InsightAssignments{Groups: map[string]string{
		"trans_market": "custom-rent",
		"trans_gone":   "custom-deleted",
	}})

	txs := map[string]models.Transaction{
		"trans_veges":  payment("trans_veges", "veges box", -40),
		"trans_rent":   payment("trans_rent", "flat rent", -510),
		"trans_bond":   payment("trans_bond", "veges", -2200),
		"trans_market": payment("trans_market", "", -60),
		"trans_atm":    {Id: "trans_atm", Type: "ATM", Amount: -100, Description: "Acu Aap9480"},
	}
	for id, tx := range txs {
		put(buckets.TransactionBucket, []byte(id), tx)
	}
	require.NoError(t, db.Close())
	return dir
}

func payment(id, reference string, amount float64) models.Transaction {
	tx := models.Transaction{Id: id, Type: "PAYMENT", Amount: amount, Description: "J SMITH"}
	tx.Meta.Reference = reference
	return tx
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	t.Setenv("BUDGE_BOLT_PATH", dir)
	store, err := NewStore()
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMigrateSpendGroups(t *testing.T) {
	dir := legacyDatabase(t)
	store := open(t, dir)

	_, err := os.Stat(filepath.Join(dir, "budge.bolt.db.pre-migration-1"))
	assert.NoError(t, err, "the database is copied aside before it is migrated")

	filed := map[string]string{}
	txs, err := store.ReadTransactions()
	require.NoError(t, err)
	for _, tx := range txs {
		filed[tx.Id] = tx.Category.Name
	}
	assert.Equal(t, "Groceries", filed["trans_veges"], "the owner's own keyword for a built in group")
	assert.Equal(t, "Rent", filed["trans_rent"], "the owner's own group")
	assert.Equal(t, "", filed["trans_bond"], "left out of the insights, it keeps Akahu's category whatever the rules say")
	assert.Equal(t, "Rent", filed["trans_market"], "put in a group by hand")
	assert.Equal(t, "Cash", filed["trans_atm"], "the built in rules a group carried come across too")

	settings, err := store.GetCategorySettings()
	require.NoError(t, err)
	var rent models.CustomCategory
	for _, c := range settings.Categories {
		if c.SpendGroup == "custom-rent" {
			rent = c
		}
	}
	assert.Equal(t, models.CustomCategory{ID: "group-custom-rent", Broad: models.SpendGroupsBroad, Specific: "Rent", SpendGroup: "custom-rent"}, rent)
	for _, rule := range settings.Rules {
		if rule.Specific == "Groceries" && rule.Match == models.MatchKeyword {
			assert.Equal(t, "veges", rule.Contains, "the owner's keywords replace the built in ones")
		}
	}
	for _, o := range settings.Overrides {
		assert.NotEqual(t, "trans_gone", o.TransactionID, "an assignment to a deleted group never counted, so it is dropped")
	}
	assert.True(t, strings.HasPrefix(settings.Rules[0].ID, groupRulePrefix))

	err = store.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(buckets.SettingsBucket)
		for _, key := range [][]byte{models.SpendRules{}.Key(), models.InsightExclusions{}.Key(), models.InsightAssignments{}.Key()} {
			assert.Nil(t, b.Get(key), "the old %s is removed", key)
		}
		version, err := schemaVersion(tx)
		assert.Equal(t, len(migrations), version)
		return err
	})
	require.NoError(t, err)
}

func TestMigrationsRunOnce(t *testing.T) {
	dir := t.TempDir()
	first := open(t, dir)
	settings, err := first.GetCategorySettings()
	require.NoError(t, err)
	assert.NotEmpty(t, settings.Rules, "a new database gets the built in groups' rules")
	require.NoError(t, first.Close())

	require.NoError(t, os.Remove(filepath.Join(dir, "budge.bolt.db.pre-migration-1")))
	again := open(t, dir)
	reopened, err := again.GetCategorySettings()
	require.NoError(t, err)
	assert.Equal(t, settings, reopened, "opening it again changes nothing")
	_, err = os.Stat(filepath.Join(dir, "budge.bolt.db.pre-migration-1"))
	assert.True(t, os.IsNotExist(err), "and makes no new copy")
}

func TestConvertSpendGroupsKeepsOwnersRulesFirst(t *testing.T) {
	mine := models.CategoryRule{ID: "abc", Contains: "LOAN INTEREST", Broad: "Debt", Specific: "Interest"}
	converted := convertSpendGroups(models.CategorySettings{Rules: []models.CategoryRule{mine}}, legacySpend{})
	assert.Equal(t, mine, converted.Rules[0], "a rule made on the categories card stays ahead")

	twice := convertSpendGroups(converted, legacySpend{})
	assert.Equal(t, converted, twice, "converting again changes nothing")
}

func TestImportOldBackupConvertsSpendGroups(t *testing.T) {
	backup := models.Backup{
		Version: models.BackupVersion,
		SpendRules: &models.SpendRules{
			Custom: []models.CustomSpendGroup{{Key: "custom-rent", Label: "Rent", SpendRule: models.SpendRule{Keywords: []string{"rent"}}}},
		},
		InsightExclusions:  []string{"trans_bond"},
		InsightAssignments: map[string]string{"trans_market": "groceries"},
	}
	store := tempStore(t)
	require.NoError(t, store.ImportBackup(backup))
	require.NoError(t, store.ImportBackup(backup))

	settings, err := store.GetCategorySettings()
	require.NoError(t, err)
	rents := 0
	for _, c := range settings.Categories {
		if c.Specific == "Rent" {
			rents++
		}
	}
	assert.Equal(t, 1, rents, "importing twice does not list a group twice")
	assert.Contains(t, settings.Overrides, models.CategoryOverride{TransactionID: "trans_bond", AkahuOnly: true})
	assert.Contains(t, settings.Overrides, models.CategoryOverride{TransactionID: "trans_market", Broad: models.SpendGroupsBroad, Specific: "Groceries"})
	assert.Len(t, settings.Overrides, 2)
}
