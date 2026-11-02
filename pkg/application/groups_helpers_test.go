package application

import (
	"slices"
	"strings"
	"testing"

	"github.com/TheQueenIsDead/budge/pkg/database"
	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/stretchr/testify/require"
)

// withGroups puts a new database's categories in place for one test: the
// built in groups and the rules the migration seeds them with. Each edit
// adjusts them first.
func withGroups(t *testing.T, edits ...func(*models.CategorySettings)) {
	t.Helper()
	t.Setenv("BUDGE_BOLT_PATH", t.TempDir())
	store, err := database.NewStore()
	require.NoError(t, err)
	settings, err := store.GetCategorySettings()
	require.NoError(t, err)
	require.NoError(t, store.Close())
	for _, edit := range edits {
		edit(&settings)
	}
	SetCategorySettings(settings)
	t.Cleanup(func() { categorySettings.Store(nil) })
}

// ownGroup adds a group of the owner's own the way the settings page does,
// its rules tried ahead of the built in ones.
func ownGroup(key, label string, keywords, merchants []string) func(*models.CategorySettings) {
	return func(s *models.CategorySettings) {
		category := models.CustomCategory{ID: key, Broad: models.SpendGroupsBroad, Specific: label, SpendGroup: key}
		s.Categories = append(s.Categories, category)
		var rules []models.CategoryRule
		for _, m := range merchants {
			rules = append(rules, models.CategoryRule{ID: newID(), Match: models.MatchMerchant, Contains: m, Broad: category.Broad, Specific: label})
		}
		for _, k := range keywords {
			rules = append(rules, models.CategoryRule{ID: newID(), Match: models.MatchKeyword, Contains: k, Broad: category.Broad, Specific: label})
		}
		s.Rules = append(s.Rules, rules...)
		// Ahead of the built in groups, as the migration lists the owner's.
		s.Categories = s.Categories[:len(s.Categories)-1]
		at := slices.IndexFunc(s.Categories, func(c models.CustomCategory) bool { return builtInGroup(c.SpendGroup) })
		if at < 0 {
			at = len(s.Categories)
		}
		s.Categories = slices.Insert(s.Categories, at, category)
		orderRules(s)
	}
}

// ownRules swaps a built in group's keywords and merchants for the owner's,
// as editing its rules on the settings page would.
func ownRules(key string, keywords, merchants []string) func(*models.CategorySettings) {
	return func(s *models.CategorySettings) {
		i := slices.IndexFunc(s.Categories, func(c models.CustomCategory) bool { return c.SpendGroup == key })
		category := s.Categories[i]
		s.Rules = slices.DeleteFunc(s.Rules, func(r models.CategoryRule) bool {
			return strings.EqualFold(r.Specific, category.Specific) && r.Match != models.MatchType
		})
		for _, k := range keywords {
			s.Rules = append(s.Rules, models.CategoryRule{ID: newID(), Match: models.MatchKeyword, Contains: k, Broad: category.Broad, Specific: category.Specific})
		}
		for _, m := range merchants {
			s.Rules = append(s.Rules, models.CategoryRule{ID: newID(), Match: models.MatchMerchant, Contains: m, Broad: category.Broad, Specific: category.Specific})
		}
		orderRules(s)
	}
}

// ownerAdded leaves out the categories and rules every database is given for
// the spend groups, so a test sees only what it added.
func ownerAdded(s models.CategorySettings) models.CategorySettings {
	s.Categories = slices.DeleteFunc(slices.Clone(s.Categories), func(c models.CustomCategory) bool { return c.SpendGroup != "" })
	s.Rules = slices.DeleteFunc(slices.Clone(s.Rules), func(r models.CategoryRule) bool { return strings.HasPrefix(r.ID, groupRulePrefix) })
	return s
}

// filed files a transaction as the store does when it reads one.
func filed(tx models.Transaction) models.Transaction {
	txs := []models.Transaction{tx}
	if settings := categorySettings.Load(); settings != nil {
		settings.Apply(txs)
	}
	return txs[0]
}

func filedAll(txs ...models.Transaction) []models.Transaction {
	out := slices.Clone(txs)
	if settings := categorySettings.Load(); settings != nil {
		settings.Apply(out)
	}
	return out
}

// classify is ClassifySpend on a transaction filed by the rules in place.
func classify(tx models.Transaction) (string, bool) { return ClassifySpend(filed(tx)) }
