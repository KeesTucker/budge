package database

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	bolt "go.etcd.io/bbolt"
)

// seedGroup is a built in spend group as it stood when spend groups moved
// onto category rules: what it was called and what, besides Akahu's
// categories, put a transaction in it. It is a frozen copy for the migration
// and must not follow later changes to the groups.
type seedGroup struct {
	key, label string
	types      []string
	merchants  []string
	keywords   []string
}

var seedGroups = []seedGroup{
	{key: "groceries", label: "Groceries", merchants: []string{"HelloFresh"}, keywords: []string{"grocer", "food", "supermarket"}},
	{key: "petrol", label: "Petrol", keywords: []string{"petrol", "fuel", "diesel", "gas"}},
	{key: "power", label: "Power", keywords: []string{"power", "electricity"}},
	{key: "connectivity", label: "Internet & Mobile", keywords: []string{"internet", "broadband", "wifi"}},
	{key: "eating-out", label: "Eating out",
		merchants: []string{"Uber Eats", "Portier", "DoorDash", "Delivereasy"},
		keywords:  []string{"takeaway", "dinner", "lunch", "brunch", "restaurant", "cafe"}},
	{key: "cash", label: "Cash", types: []string{"ATM"}},
}

// groupRulePrefix starts the ID of every rule made for a spend group, so the
// block can be told apart from rules the owner added on the categories card.
const groupRulePrefix = "group-"

// legacySpend is how spend groups were kept before they moved onto rules.
type legacySpend struct {
	rules       models.SpendRules
	exclusions  []string
	assignments map[string]string
}

func (l legacySpend) empty() bool {
	return len(l.rules.Groups) == 0 && len(l.rules.Custom) == 0 && len(l.exclusions) == 0 && len(l.assignments) == 0
}

// convertSpendGroups folds spend groups into category settings:
//
//   - every group, built in or the owner's, gets a category of its own under
//     "Your groups" that feeds it, and its keywords, merchants and types become
//     rules filing there. The owner's groups go first, as they were tried
//     first. Any group rules already there are replaced, and rules the owner
//     added on the categories card stay ahead of them.
//   - a transaction put in a group by hand becomes an override to that
//     group's category, and one left out of the insights an override that
//     keeps Akahu's category beyond every rule. A transaction that already
//     has an override keeps it.
func convertSpendGroups(current models.CategorySettings, legacy legacySpend) models.CategorySettings {
	type group struct {
		key, label                 string
		types, merchants, keywords []string
	}
	var groups []group
	for _, custom := range legacy.rules.Custom {
		groups = append(groups, group{key: custom.Key, label: custom.Label, merchants: custom.Merchants, keywords: custom.Keywords})
	}
	for _, seed := range seedGroups {
		g := group(seed)
		if edited, ok := legacy.rules.Groups[seed.key]; ok {
			g.merchants, g.keywords = edited.Merchants, edited.Keywords
		}
		groups = append(groups, g)
	}

	labels := make(map[string]string, len(groups))
	for _, g := range groups {
		labels[g.key] = g.label
		i := slices.IndexFunc(current.Categories, func(c models.CustomCategory) bool { return strings.EqualFold(c.Specific, g.label) })
		if i >= 0 {
			current.Categories[i].SpendGroup = g.key
			continue
		}
		current.Categories = append(current.Categories, models.CustomCategory{
			ID: groupRulePrefix + g.key, Broad: models.SpendGroupsBroad, Specific: g.label, SpendGroup: g.key,
		})
	}

	rules := slices.DeleteFunc(slices.Clone(current.Rules), func(r models.CategoryRule) bool {
		return strings.HasPrefix(r.ID, groupRulePrefix)
	})
	// Types first, then merchants, then keywords, as a transaction was tried.
	add := func(g group, match string, values []string) {
		for i, value := range values {
			if strings.TrimSpace(value) == "" {
				continue
			}
			rule := models.CategoryRule{
				ID:       fmt.Sprintf("%s%s-%s-%d", groupRulePrefix, g.key, match, i),
				Match:    match,
				Contains: value,
				Broad:    models.SpendGroupsBroad,
				Specific: g.label,
			}
			if match == models.MatchType {
				rule.Contains, rule.Type = "", value
			}
			rules = append(rules, rule)
		}
	}
	for _, g := range groups {
		add(g, models.MatchType, g.types)
	}
	for _, g := range groups {
		add(g, models.MatchMerchant, g.merchants)
	}
	for _, g := range groups {
		add(g, models.MatchKeyword, g.keywords)
	}
	current.Rules = rules

	overridden := make(map[string]bool, len(current.Overrides))
	for _, o := range current.Overrides {
		overridden[o.TransactionID] = true
	}
	for _, id := range legacy.exclusions {
		if id != "" && !overridden[id] {
			overridden[id] = true
			current.Overrides = append(current.Overrides, models.CategoryOverride{TransactionID: id, AkahuOnly: true})
		}
	}
	ids := make([]string, 0, len(legacy.assignments))
	for id := range legacy.assignments {
		ids = append(ids, id)
	}
	slices.Sort(ids) // a map has no order, and the settings should come out the same every time
	for _, id := range ids {
		label, known := labels[legacy.assignments[id]]
		// An assignment to a deleted group never counted, so it is dropped.
		if id == "" || overridden[id] || !known {
			continue
		}
		overridden[id] = true
		current.Overrides = append(current.Overrides, models.CategoryOverride{
			TransactionID: id, Broad: models.SpendGroupsBroad, Specific: label,
		})
	}
	return current
}

// migrateSpendGroups moves spend groups onto category rules, then removes
// the old documents. It runs on a new database too, which is how a new
// instance gets the built in groups' rules.
func migrateSpendGroups(tx *bolt.Tx) error {
	var legacy legacySpend
	rules, _, err := getIn[models.SpendRules](tx, models.SpendRules{}.Key())
	if err != nil {
		return err
	}
	legacy.rules = rules
	exclusions, _, err := getIn[models.InsightExclusions](tx, models.InsightExclusions{}.Key())
	if err != nil {
		return err
	}
	legacy.exclusions = exclusions.TransactionIDs
	assignments, _, err := getIn[models.InsightAssignments](tx, models.InsightAssignments{}.Key())
	if err != nil {
		return err
	}
	legacy.assignments = assignments.Groups

	current, _, err := getIn[models.CategorySettings](tx, models.CategorySettings{}.Key())
	if err != nil {
		return err
	}
	if err := putAll(tx, []models.CategorySettings{convertSpendGroups(current, legacy)}); err != nil {
		return err
	}

	settings := tx.Bucket(models.SpendRules{}.Bucket())
	for _, key := range [][]byte{models.SpendRules{}.Key(), models.InsightExclusions{}.Key(), models.InsightAssignments{}.Key()} {
		if err := settings.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
