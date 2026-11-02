package application

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// ScheduleView is the schedule as the settings page reads it: what is on, how
// often, and when it will next happen.
type ScheduleView struct {
	Settings       models.ScheduleSettings
	AkahuIntervals []models.ScheduleInterval
	AssetIntervals []models.ScheduleInterval

	// Next is zero when a job has never run, in which case it is due at the next
	// tick and the page says so rather than showing a date in the past.
	AkahuNext  time.Time
	AssetsNext time.Time
}

func buildScheduleView(schedule models.ScheduleSettings) ScheduleView {
	view := ScheduleView{
		Settings:       schedule,
		AkahuIntervals: models.AkahuIntervals,
		AssetIntervals: models.AssetIntervals,
	}
	if !schedule.AkahuLastRun.IsZero() {
		view.AkahuNext = schedule.AkahuLastRun.Add(schedule.AkahuEvery().Every)
	}
	if !schedule.AssetsLastRun.IsZero() {
		view.AssetsNext = schedule.AssetsLastRun.Add(schedule.AssetsEvery().Every)
	}
	return view
}

func (app *Application) Settings(c echo.Context) error {
	accounts, err := app.store.ReadAccounts()
	if err != nil {
		app.Toast(c, "Error", "Could not read accounts.")
		return c.NoContent(http.StatusInternalServerError)
	}

	akahuConfig, err := app.store.GetAkahuSettings()
	if err != nil {
		app.Toast(c, "Error", "Could not get Akahu settings.")
		return c.NoContent(http.StatusInternalServerError)
	}

	schedule, err := app.store.GetScheduleSettings()
	if err != nil {
		app.Toast(c, "Error", "Could not read the schedule.")
		return c.NoContent(http.StatusInternalServerError)
	}

	removed, err := app.store.GetRemovedAccounts()
	if err != nil {
		app.Toast(c, "Error", "Could not read untracked accounts.")
		return c.NoContent(http.StatusInternalServerError)
	}

	categories, err := app.buildCategoriesView()
	if err != nil {
		app.Toast(c, "Error", "Could not read category rules.")
		return c.NoContent(http.StatusInternalServerError)
	}

	return c.Render(http.StatusOK, "settings", map[string]interface{}{
		"accounts":       accounts,
		"removed":        removed.Accounts,
		"restoring":      removed.Restoring(),
		"spendRules":     spendRuleViews(),
		"categories":     categories,
		"akahuAppToken":  akahuConfig.AppToken,
		"akahuUserToken": akahuConfig.UserToken,
		"sync":           app.syncStatus(),
		"schedule":       buildScheduleView(schedule),
	})
}

func (app *Application) SettingsDeleteSynced(c echo.Context) error {

	err := app.store.DeleteSynced()
	if err != nil {
		return err
	}
	_ = app.store.ResetAkahuLastSync()

	app.Toast(c, "Success", "All synced data removed.")
	return nil
}

func (app *Application) PutAkahuSettings(c echo.Context) error {

	settings, err := app.store.GetAkahuSettings()
	if err != nil {
		return err
	}

	settings.AppToken = c.FormValue("akahuAppToken")
	settings.UserToken = c.FormValue("akahuUserToken")
	if err := settings.Validate(); err != nil {
		return err
	}

	err = app.integrations.PutAkahuSettings(settings)
	if err != nil {
		return err
	}
	app.Toast(c, "Success", "Akahu settings saved successfully!")
	return nil
}

// SettingsSaveSchedule stores what should run by itself. Last-run times are
// carried across rather than reset, so changing a cadence does not fire a job
// immediately as a side effect of saving.
func (app *Application) SettingsSaveSchedule(c echo.Context) error {

	schedule, err := app.store.GetScheduleSettings()
	if err != nil {
		app.Toast(c, "Error", "Could not read the schedule.")
		return c.NoContent(http.StatusInternalServerError)
	}

	schedule.AkahuEnabled = c.FormValue("akahu_enabled") != ""
	schedule.AssetsEnabled = c.FormValue("assets_enabled") != ""
	schedule.AkahuInterval = models.ResolveInterval(
		models.AkahuIntervals, c.FormValue("akahu_interval"), schedule.AkahuEvery()).Key
	schedule.AssetsInterval = models.ResolveInterval(
		models.AssetIntervals, c.FormValue("assets_interval"), schedule.AssetsEvery()).Key

	if err := app.store.SaveScheduleSettings(schedule); err != nil {
		app.Toast(c, "Error", "Could not save the schedule.")
		return c.NoContent(http.StatusInternalServerError)
	}

	app.Toast(c, "Success", "Schedule saved.")
	return c.Render(http.StatusOK, "settings.schedule", buildScheduleView(schedule))
}

// renderSettingsAccounts answers a tracking switch with the accounts card.
func (app *Application) renderSettingsAccounts(c echo.Context) error {
	accounts, err := app.store.ReadAccounts()
	if err != nil {
		app.Toast(c, "Error", "Could not read accounts.")
		return c.NoContent(http.StatusInternalServerError)
	}
	removed, err := app.store.GetRemovedAccounts()
	if err != nil {
		app.Toast(c, "Error", "Could not read untracked accounts.")
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.Render(http.StatusOK, "settings.accounts", map[string]interface{}{
		"accounts":  accounts,
		"removed":   removed.Accounts,
		"restoring": removed.Restoring(),
	})
}

// SettingsAccounts answers the accounts card's poll while an account is on
// its way back.
func (app *Application) SettingsAccounts(c echo.Context) error {
	return app.renderSettingsAccounts(c)
}

// SettingsUntrackAccount stops tracking an account Akahu shares, such as one
// held jointly or for someone else, dropping what has been synced from it.
func (app *Application) SettingsUntrackAccount(c echo.Context) error {
	if err := app.store.RemoveAccount(c.Param("id")); err != nil {
		app.Toast(c, "Error", "Could not stop tracking the account.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Success", "Account no longer tracked.")
	return app.renderSettingsAccounts(c)
}

// SettingsTrackAccount tracks an account again and starts the sync that
// brings back its history. If a sync is already running it is left to
// finish, and the next one fetches the account.
func (app *Application) SettingsTrackAccount(c echo.Context) error {
	if err := app.store.RestoreAccount(c.Param("id")); err != nil {
		app.Toast(c, "Error", "Could not track the account.")
		return c.NoContent(http.StatusInternalServerError)
	}
	if app.akahuSync.begin() {
		go func() {
			if err := app.runAkahuSync(); err != nil {
				app.Notify(models.NotificationError, SourceAkahuSync, "Sync failed", err.Error())
			}
		}()
		app.Toast(c, "Success", "Account tracked again. Syncing its history back.")
	} else {
		app.Toast(c, "Success", "Account tracked again. It comes back with the next sync.")
	}
	return app.renderSettingsAccounts(c)
}

// SpendRuleView is one spend group's rules as the settings page edits them:
// the keyword and merchant rules filing into the group's own category.
type SpendRuleView struct {
	Group     SpendGroup
	Keywords  string
	Merchants string

	// Own marks a group the owner made, which can be deleted.
	Own bool
}

// groupRulePrefix starts the ID of every rule made for a spend group, as the
// migration that moved groups onto rules named them.
const groupRulePrefix = "group-"

func groupRuleID(key, match string) string {
	return groupRulePrefix + key + "-" + match + "-" + newID()
}

// groupCategory is the custom category a spend group's rules file into.
func groupCategory(settings models.CategorySettings, key string) (models.CustomCategory, bool) {
	for _, category := range settings.Categories {
		if category.SpendGroup == key {
			return category, true
		}
	}
	return models.CustomCategory{}, false
}

// groupRules lists what a group's rules of one kind look for.
func groupRules(settings models.CategorySettings, category models.CustomCategory, match string) []string {
	var values []string
	for _, rule := range settings.Rules {
		if rule.Match == match && strings.EqualFold(rule.Specific, category.Specific) {
			values = append(values, rule.Contains)
		}
	}
	return values
}

func spendRuleViews() []SpendRuleView {
	var settings models.CategorySettings
	if loaded := categorySettings.Load(); loaded != nil {
		settings = *loaded
	}
	groups := SpendGroups()
	views := make([]SpendRuleView, len(groups))
	for i, group := range groups {
		view := SpendRuleView{Group: group, Own: !builtInGroup(group.Key)}
		if category, ok := groupCategory(settings, group.Key); ok {
			view.Keywords = strings.Join(groupRules(settings, category, models.MatchKeyword), ", ")
			view.Merchants = strings.Join(groupRules(settings, category, models.MatchMerchant), ", ")
		}
		views[i] = view
	}
	return views
}

// replaceGroupRules swaps a group's rules of one kind for new ones, in the
// place the old ones held, so the order rules are tried in stays put.
func replaceGroupRules(rules []models.CategoryRule, category models.CustomCategory, match string, values []string) []models.CategoryRule {
	at := -1
	kept := make([]models.CategoryRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Match == match && strings.EqualFold(rule.Specific, category.Specific) {
			if at < 0 {
				at = len(kept)
			}
			continue
		}
		kept = append(kept, rule)
	}
	fresh := make([]models.CategoryRule, 0, len(values))
	for _, value := range values {
		fresh = append(fresh, models.CategoryRule{
			ID: groupRuleID(category.SpendGroup, match), Match: match, Contains: value,
			Broad: category.Broad, Specific: category.Specific,
		})
	}
	if at < 0 {
		return append(kept, fresh...)
	}
	return slices.Concat(kept[:at], fresh, kept[at:])
}

// parseRuleList reads a comma or line separated list as typed into a field,
// dropping blanks and repeats however they are cased.
func parseRuleList(text string) []string {
	seen := make(map[string]bool)
	list := []string{}
	for _, item := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '\n' }) {
		item = strings.Join(strings.Fields(item), " ")
		if item == "" || seen[strings.ToLower(item)] {
			continue
		}
		seen[strings.ToLower(item)] = true
		list = append(list, item)
	}
	return list
}

// SettingsSaveSpendRule stores the keywords and merchants a spend group
// matches on, as rules filing into the group's category. A built in group
// whose category was deleted gets it back.
func (app *Application) SettingsSaveSpendRule(c echo.Context) error {
	group, ok := SpendGroupByKey(c.Param("key"))
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such spend group")
	}
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	category, ok := groupCategory(settings, group.Key)
	if !ok {
		category = models.CustomCategory{ID: newID(), Broad: models.SpendGroupsBroad, Specific: group.Label, SpendGroup: group.Key}
		settings.Categories = append(settings.Categories, category)
	}
	settings.Rules = replaceGroupRules(settings.Rules, category, models.MatchKeyword, parseRuleList(c.FormValue("keywords")))
	settings.Rules = replaceGroupRules(settings.Rules, category, models.MatchMerchant, parseRuleList(c.FormValue("merchants")))
	if err := app.store.SaveCategorySettings(settings); err != nil {
		app.Toast(c, "Error", "Could not save the insight rules.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Success", "Insight rules saved.")
	return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
}

// customGroupPrefix starts the key of every group the owner makes.
const customGroupPrefix = "custom-"

// SettingsAddSpendGroup makes a new group of the owner's own, empty until its
// keywords and merchants are filled in.
func (app *Application) SettingsAddSpendGroup(c echo.Context) error {
	label := strings.Join(strings.Fields(c.FormValue("label")), " ")
	if label == "" {
		app.Toast(c, "Error", "Give the group a name.")
		return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
	}
	key := customGroupPrefix + lettersOnly(label)
	if key == customGroupPrefix {
		key = customGroupPrefix + newID()
	}
	if _, taken := SpendGroupByKey(key); taken {
		app.Toast(c, "Error", "There is already a group called "+label+".")
		return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
	}

	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	// Budget targets are keyed by the category name alone, so a group cannot
	// share its name with a category that already exists.
	if slices.ContainsFunc(settings.Categories, func(c models.CustomCategory) bool { return strings.EqualFold(c.Specific, label) }) {
		app.Toast(c, "Error", "There is already a category called "+label+".")
		return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
	}
	settings.Categories = append(settings.Categories, models.CustomCategory{
		ID: newID(), Broad: models.SpendGroupsBroad, Specific: label, SpendGroup: key,
	})
	if err := app.store.SaveCategorySettings(settings); err != nil {
		app.Toast(c, "Error", "Could not add the group.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Success", label+" added. Give it keywords or merchants to fill it.")
	return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
}

// SettingsDeleteSpendGroup removes one of the owner's own groups, with its
// category and the rules filing into it. What they matched goes back to
// Akahu's category, or to another rule.
func (app *Application) SettingsDeleteSpendGroup(c echo.Context) error {
	key := c.Param("key")
	if builtInGroup(key) {
		return echo.NewHTTPError(http.StatusBadRequest, "built in groups cannot be deleted")
	}
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	if category, ok := groupCategory(settings, key); ok {
		settings.Categories = slices.DeleteFunc(settings.Categories, func(c models.CustomCategory) bool { return c.SpendGroup == key })
		settings.Rules = slices.DeleteFunc(settings.Rules, func(r models.CategoryRule) bool {
			return strings.EqualFold(r.Specific, category.Specific)
		})
	}
	if err := app.store.SaveCategorySettings(settings); err != nil {
		app.Toast(c, "Error", "Could not delete the group.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Success", "Group deleted.")
	return c.Render(http.StatusOK, "settings.spend", spendRuleViews())
}
