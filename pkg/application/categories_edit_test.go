package application

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// categoryAction runs a categories handler against one category or rule and
// returns the card it answers with.
func categoryAction(t *testing.T, app *Application, handler echo.HandlerFunc, id string, values url.Values) string {
	t.Helper()
	c, rec := postForm(t, app, values)
	c.SetParamNames("id")
	c.SetParamValues(id)
	require.NoError(t, handler(c))
	return rec.Body.String()
}

func ownCategory(t *testing.T, app *Application, specific string) models.CustomCategory {
	t.Helper()
	settings, err := app.store.GetCategorySettings()
	require.NoError(t, err)
	i := slices.IndexFunc(settings.Categories, func(c models.CustomCategory) bool { return c.Specific == specific })
	require.GreaterOrEqual(t, i, 0, "no category %s", specific)
	return settings.Categories[i]
}

func TestRenamingACategoryTakesEverythingWithIt(t *testing.T) {
	app := newTestApp(t)
	recent := time.Now().Add(-48 * time.Hour)
	rent := referenced("PAYMENT", "", "flat rent", -510)
	rent.Id, rent.Date = "rent", recent
	byHand := referenced("PAYMENT", "", "", -40)
	byHand.Id, byHand.Date = "hand", recent
	require.NoError(t, app.store.CreateTransaction(rent))
	require.NoError(t, app.store.CreateTransaction(byHand))

	c, _ := postForm(t, app, url.Values{"specific": {"Rent"}, "insights": {"1"}})
	require.NoError(t, app.SettingsAddCategory(c))
	category := ownCategory(t, app, "Rent")
	assert.Equal(t, models.SpendGroupsBroad, category.Broad, "a blank group on Insights goes under Your groups")
	assert.Equal(t, "custom-rent", category.SpendGroup)

	c, _ = postForm(t, app, url.Values{"category": {"Rent"}, "match": {"keyword"}, "contains": {"rent"}})
	require.NoError(t, app.SettingsAddRule(c))
	c, _ = postForm(t, app, url.Values{"category": {"Rent"}})
	c.SetParamNames("id")
	c.SetParamValues("hand")
	require.NoError(t, app.TransactionSetCategory(c))
	require.NoError(t, app.store.CreateBudgetItem(models.BudgetItem{ID: "b", Category: "Rent", BroadCategory: models.SpendGroupsBroad, Amount: 510, Frequency: "weekly"}))

	categoryAction(t, app, app.SettingsUpdateCategory, category.ID, url.Values{"broad": {"Housing"}, "specific": {"Flat"}, "insights": {"1"}})

	for _, id := range []string{"rent", "hand"} {
		tx, err := app.store.GetTransaction(id)
		require.NoError(t, err)
		assert.Equal(t, "Flat", tx.Category.Name, "%s follows the rename", id)
		key, ok := ClassifySpend(tx)
		assert.True(t, ok)
		assert.Equal(t, "custom-rent", key, "and stays in the same spend group")
	}
	item, err := app.store.GetBudgetItem("b")
	require.NoError(t, err)
	assert.Equal(t, "Flat", item.Category, "the budget target moves with it")
	assert.Equal(t, "Housing", item.BroadCategory)

	categoryAction(t, app, app.SettingsUpdateCategory, category.ID, url.Values{"broad": {"Housing"}, "specific": {"Flat"}})
	assert.Empty(t, ownCategory(t, app, "Flat").SpendGroup, "unticked, it leaves Insights")
	_, ok := SpendGroupByKey("custom-rent")
	assert.False(t, ok)
}

func TestBuiltInGroupsKeepTheirCategory(t *testing.T) {
	app := newTestApp(t)
	groceries := ownCategory(t, app, "Groceries")

	body := categoryAction(t, app, app.SettingsUpdateCategory, groceries.ID, url.Values{"broad": {"Food"}, "specific": {"Shopping"}})
	assert.Contains(t, body, "keeps its name")
	body = categoryAction(t, app, app.SettingsDeleteCategory, groceries.ID, nil)
	assert.Contains(t, body, "cannot be deleted")
	assert.Equal(t, groceries, ownCategory(t, app, "Groceries"))
}

func TestMovingACategoryDecidesBetweenRulesOfAKind(t *testing.T) {
	app := newTestApp(t)
	tx := referenced("PAYMENT", "", "lunch food", -20)
	tx.Id, tx.Date = "lunch", time.Now().Add(-time.Hour)
	require.NoError(t, app.store.CreateTransaction(tx))

	c, _ := postForm(t, app, url.Values{"specific": {"Lunches"}, "insights": {"1"}})
	require.NoError(t, app.SettingsAddCategory(c))
	c, _ = postForm(t, app, url.Values{"category": {"Lunches"}, "match": {"keyword"}, "contains": {"lunch"}})
	require.NoError(t, app.SettingsAddRule(c))

	group := func() string {
		read, err := app.store.GetTransaction("lunch")
		require.NoError(t, err)
		key, _ := ClassifySpend(read)
		return key
	}
	assert.Equal(t, "custom-lunches", group(), "a new category goes ahead of the built in groups, so its keyword wins")

	lunches := ownCategory(t, app, "Lunches")
	for range 20 {
		categoryAction(t, app, app.SettingsMoveCategory, lunches.ID, url.Values{"direction": {"down"}})
	}
	settings, err := app.store.GetCategorySettings()
	require.NoError(t, err)
	assert.Equal(t, "Lunches", settings.Categories[len(settings.Categories)-1].Specific, "moving down stops at the bottom")
	assert.Equal(t, "groceries", group(), "below groceries, the groceries keyword wins")
}

func TestEditingARule(t *testing.T) {
	app := newTestApp(t)
	settings, err := app.store.GetCategorySettings()
	require.NoError(t, err)
	i := slices.IndexFunc(settings.Rules, func(r models.CategoryRule) bool { return r.Contains == "HelloFresh" })
	require.GreaterOrEqual(t, i, 0)
	rule := settings.Rules[i]

	body := categoryAction(t, app, app.SettingsUpdateRule, rule.ID, url.Values{"match": {"merchant"}})
	assert.Contains(t, body, "needs some text", "an edit cannot empty a rule")

	categoryAction(t, app, app.SettingsUpdateRule, rule.ID, url.Values{"match": {"keyword"}, "contains": {" hello fresh "}})
	settings, err = app.store.GetCategorySettings()
	require.NoError(t, err)
	j := slices.IndexFunc(settings.Rules, func(r models.CategoryRule) bool { return r.ID == rule.ID })
	edited := settings.Rules[j]
	assert.Equal(t, models.MatchKeyword, edited.Match)
	assert.Equal(t, "hello fresh", edited.Contains)
	assert.Equal(t, rule.Specific, edited.Specific, "it still files under Groceries")
	assert.True(t, strings.HasPrefix(edited.ID, groupRulePrefix), "and keeps its id")
}
