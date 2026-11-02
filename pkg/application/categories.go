package application

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// CategoryOption is one category a rule can file transactions under.
type CategoryOption struct {
	Broad    string
	Specific string
	Custom   bool
}

// CategoryOptionGroup is the options under one broad category, for an
// <optgroup> in a category picker.
type CategoryOptionGroup struct {
	Broad   string
	Options []CategoryOption
}

// RuleView is a rule with how many transactions it currently files, so a
// rule that matches nothing stands out as a typo.
type RuleView struct {
	models.CategoryRule
	Matches int
}

// CategoryBlock is one category on the categories card, with the rules that
// file into it.
type CategoryBlock struct {
	// Key names the block in the page and in URLs: the custom category's ID,
	// or the name of an Akahu category that rules file into.
	Key      string
	DomID    string
	Broad    string
	Specific string
	// Custom is a category of the owner's, which can be renamed, moved and
	// deleted. BuiltIn is a built in spend group's own category, which keeps
	// its name and stays, since the group is defined in code.
	Custom  bool
	BuiltIn bool
	// Group is the spend group the category feeds on the insights page.
	Group   *SpendGroup
	Rules   []RuleView
	Matches int
	// First and Last say where it sits among the owner's categories, for the
	// move buttons.
	First, Last bool
	Open        bool
}

// CategoriesView is the view-model for the settings.categories partial.
type CategoriesView struct {
	Blocks []CategoryBlock
	// Akahu is the catalogue of Akahu's categories, for adding a rule to one
	// that has none yet. Broads names every broad category, for the new
	// category form.
	Akahu  []CategoryOptionGroup
	Broads []string
	Types  []string            // transaction types seen, for narrowing a rule
	Blank  models.CategoryRule // what a new rule's fields start from
	// Suggestions autofill a rule's match text, uncategorised first.
	Suggestions []string
	Error       string
}

// maxRuleSuggestions bounds the autofill list; past a few hundred the browser
// list is too long to be useful and typing narrows it anyway.
const maxRuleSuggestions = 300

// ruleSuggestions offers match text for a new rule: each transaction's
// merchant, or its description cleaned of card terminal noise. The cleaning
// only trims, so a suggestion is always text the transaction contains and a
// rule built from it matches. Transactions a rule already files are left out,
// and uncategorised ones come first, being what rules are mostly for. Money
// leaving by transfer is offered too, for rules that count it, but never the
// leg where it arrives, which no rule can match.
func ruleSuggestions(txs []models.Transaction, settings models.CategorySettings) []string {
	type suggestion struct {
		text          string
		count         int
		uncategorised bool
	}
	byText := make(map[string]*suggestion)
	for _, tx := range txs {
		if (tx.IsTransfer() && tx.Amount >= 0) || settings.MatchingRule(tx) >= 0 {
			continue
		}
		text := strings.TrimSpace(spendMerchant(tx))
		if text == "" {
			continue
		}
		key := strings.ToLower(text)
		if byText[key] == nil {
			byText[key] = &suggestion{text: text}
		}
		byText[key].count++
		if tx.Category.Groups.PersonalFinance.Name == "" {
			byText[key].uncategorised = true
		}
	}
	all := make([]*suggestion, 0, len(byText))
	for _, s := range byText {
		all = append(all, s)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].uncategorised != all[j].uncategorised {
			return all[i].uncategorised
		}
		if all[i].count != all[j].count {
			return all[i].count > all[j].count
		}
		return all[i].text < all[j].text
	})
	if len(all) > maxRuleSuggestions {
		all = all[:maxRuleSuggestions]
	}
	texts := make([]string, len(all))
	for i, s := range all {
		texts[i] = s.text
	}
	return texts
}

// categoryCatalogue lists every category a rule can target: the custom ones
// first, then each pair Akahu has used. Transfers are left out since no rule
// applies to them. Specific names are the key budget items match on, so the
// catalogue is keyed by them, lower-cased.
func categoryCatalogue(custom []models.CustomCategory, txs []models.Transaction) map[string]CategoryOption {
	catalogue := make(map[string]CategoryOption)
	for _, category := range custom {
		catalogue[strings.ToLower(category.Specific)] = CategoryOption{Broad: category.Broad, Specific: category.Specific, Custom: true}
	}
	for _, tx := range txs {
		broad := tx.Category.Groups.PersonalFinance.Name
		if broad == "" || tx.IsTransfer() {
			continue
		}
		specific := tx.Category.Name
		if specific == "" {
			specific = broad
		}
		if _, seen := catalogue[strings.ToLower(specific)]; !seen {
			catalogue[strings.ToLower(specific)] = CategoryOption{Broad: broad, Specific: specific}
		}
	}
	return catalogue
}

// categoryOptionGroups lays a catalogue out for a category picker: grouped by
// broad category, each group and its options in alphabetical order.
func categoryOptionGroups(catalogue map[string]CategoryOption) []CategoryOptionGroup {
	byBroad := make(map[string][]CategoryOption)
	for _, option := range catalogue {
		byBroad[option.Broad] = append(byBroad[option.Broad], option)
	}
	var groups []CategoryOptionGroup
	for broad, options := range byBroad {
		sort.Slice(options, func(i, j int) bool { return options[i].Specific < options[j].Specific })
		groups = append(groups, CategoryOptionGroup{Broad: broad, Options: options})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Broad < groups[j].Broad })
	return groups
}

// orderRules sorts rules into category order: the owner's categories in the
// order they are listed, then Akahu's, in the order their first rule was
// added. Within a match tier the first rule listed wins, so this is what
// moving a category up or down changes. The sort is stable, so rules keep
// their order within a category.
func orderRules(settings *models.CategorySettings) {
	rank := make(map[string]int, len(settings.Categories))
	for i, category := range settings.Categories {
		rank[strings.ToLower(category.Specific)] = i
	}
	next := len(settings.Categories)
	for _, rule := range settings.Rules {
		if _, ok := rank[strings.ToLower(rule.Specific)]; !ok {
			rank[strings.ToLower(rule.Specific)] = next
			next++
		}
	}
	slices.SortStableFunc(settings.Rules, func(a, b models.CategoryRule) int {
		return rank[strings.ToLower(a.Specific)] - rank[strings.ToLower(b.Specific)]
	})
}

// saveCategories stores the category settings with their rules in category
// order.
func (app *Application) saveCategories(settings models.CategorySettings) error {
	orderRules(&settings)
	return app.store.SaveCategorySettings(settings)
}

func (app *Application) buildCategoriesView(open string) (CategoriesView, error) {
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return CategoriesView{}, err
	}
	orderRules(&settings)
	txs, err := app.store.ReadTransactions()
	if err != nil {
		return CategoriesView{}, err
	}

	view := CategoriesView{Suggestions: ruleSuggestions(txs, settings)}

	// Rules match on description, merchant and type, none of which a rule
	// changes, so matching the already-categorised copies counts correctly.
	matches := make([]int, len(settings.Rules))
	types := make(map[string]bool)
	for _, tx := range txs {
		if n := settings.MatchingRule(tx); n >= 0 {
			matches[n]++
		}
		if tx.Type != "" && !tx.IsTransfer() {
			types[tx.Type] = true
		}
	}
	for t := range types {
		view.Types = append(view.Types, t)
	}
	sort.Strings(view.Types)

	index := make(map[string]int)
	block := func(key, broad, specific string) *CategoryBlock {
		lower := strings.ToLower(specific)
		if i, ok := index[lower]; ok {
			return &view.Blocks[i]
		}
		b := CategoryBlock{Key: key, DomID: sanitizeWithPrefix("catblk-", key), Broad: broad, Specific: specific, Open: key == open}
		if groupKey, ok := groupOfCategory(specific); ok {
			if group, found := SpendGroupByKey(groupKey); found {
				b.Group = &group
			}
		}
		index[lower] = len(view.Blocks)
		view.Blocks = append(view.Blocks, b)
		return &view.Blocks[len(view.Blocks)-1]
	}
	for i, category := range settings.Categories {
		b := block(category.ID, category.Broad, category.Specific)
		b.Custom = true
		b.BuiltIn = builtInGroup(category.SpendGroup)
		b.First, b.Last = i == 0, i == len(settings.Categories)-1
	}
	for i, rule := range settings.Rules {
		b := block(rule.Specific, rule.Broad, rule.Specific)
		b.Rules = append(b.Rules, RuleView{CategoryRule: rule, Matches: matches[i]})
		b.Matches += matches[i]
	}

	catalogue := categoryCatalogue(settings.Categories, txs)
	broads := make(map[string]bool)
	akahu := make(map[string]CategoryOption)
	for key, option := range catalogue {
		broads[option.Broad] = true
		if !option.Custom {
			akahu[key] = option
		}
	}
	view.Akahu = categoryOptionGroups(akahu)
	for broad := range broads {
		view.Broads = append(view.Broads, broad)
	}
	sort.Strings(view.Broads)
	return view, nil
}

// renderCategories answers every categories action with the whole card, so
// rule counts and every picker reflect the change. open keeps the category
// that was acted on open; a message explains an action that was refused.
func (app *Application) renderCategories(c echo.Context, open, message string) error {
	view, err := app.buildCategoriesView(open)
	if err != nil {
		return err
	}
	view.Error = message
	return c.Render(http.StatusOK, "settings.categories", view)
}

// insightsKey is the spend group key a category of the owner's gets when it
// is put on the insights page.
func insightsKey(specific string) string {
	key := customGroupPrefix + lettersOnly(specific)
	if key == customGroupPrefix {
		key = customGroupPrefix + newID()
	}
	return key
}

// customGroupPrefix starts the key of every spend group the owner makes.
const customGroupPrefix = "custom-"

// nameTaken reports whether another category already uses a name. Budget
// targets are keyed by the name alone, so two categories sharing one would
// share a target.
func (app *Application) nameTaken(settings models.CategorySettings, specific, except string) (CategoryOption, bool, error) {
	txs, err := app.store.ReadTransactions()
	if err != nil {
		return CategoryOption{}, false, err
	}
	others := slices.DeleteFunc(slices.Clone(settings.Categories), func(c models.CustomCategory) bool { return c.ID == except })
	existing, taken := categoryCatalogue(others, txs)[strings.ToLower(specific)]
	return existing, taken, nil
}

func (app *Application) SettingsAddCategory(c echo.Context) error {
	broad := strings.TrimSpace(c.FormValue("broad"))
	specific := strings.TrimSpace(c.FormValue("specific"))
	insights := c.FormValue("insights") != ""
	if broad == "" && insights {
		broad = models.SpendGroupsBroad
	}
	if broad == "" || specific == "" {
		return app.renderCategories(c, "", "A category needs both a group and a name.")
	}

	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	if existing, taken, err := app.nameTaken(settings, specific, ""); err != nil {
		return err
	} else if taken {
		return app.renderCategories(c, "", "“"+specific+"” already exists under "+existing.Broad+".")
	}

	category := models.CustomCategory{ID: newID(), Broad: broad, Specific: specific}
	if insights {
		category.SpendGroup = insightsKey(specific)
	}
	// Ahead of the built in groups, so a narrow group of the owner's, such
	// as lunches, takes what it matches from the broad ones.
	at := slices.IndexFunc(settings.Categories, func(c models.CustomCategory) bool { return builtInGroup(c.SpendGroup) })
	if at < 0 {
		at = len(settings.Categories)
	}
	settings.Categories = slices.Insert(settings.Categories, at, category)
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, category.ID, "")
}

// SettingsUpdateCategory renames one of the owner's categories, or puts it on
// or takes it off the insights page. A rename carries the rules, the
// transactions filed by hand and the budget target with it. A built in
// group's category keeps its name and its place on the insights page.
func (app *Application) SettingsUpdateCategory(c echo.Context) error {
	id := c.Param("id")
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(settings.Categories, func(c models.CustomCategory) bool { return c.ID == id })
	if i < 0 {
		return echo.NewHTTPError(http.StatusNotFound, "No such category.")
	}
	old := settings.Categories[i]
	if builtInGroup(old.SpendGroup) {
		return app.renderCategories(c, id, "A built in group keeps its name.")
	}

	next := old
	next.Broad = strings.TrimSpace(c.FormValue("broad"))
	next.Specific = strings.TrimSpace(c.FormValue("specific"))
	if next.Broad == "" || next.Specific == "" {
		return app.renderCategories(c, id, "A category needs both a group and a name.")
	}
	if !strings.EqualFold(next.Specific, old.Specific) {
		if existing, taken, err := app.nameTaken(settings, next.Specific, id); err != nil {
			return err
		} else if taken {
			return app.renderCategories(c, id, "“"+next.Specific+"” already exists under "+existing.Broad+".")
		}
	}
	switch insights := c.FormValue("insights") != ""; {
	case insights && next.SpendGroup == "":
		next.SpendGroup = insightsKey(next.Specific)
	case !insights:
		next.SpendGroup = ""
	}
	settings.Categories[i] = next

	for j := range settings.Rules {
		if strings.EqualFold(settings.Rules[j].Specific, old.Specific) {
			settings.Rules[j].Broad, settings.Rules[j].Specific = next.Broad, next.Specific
		}
	}
	for j := range settings.Overrides {
		if !settings.Overrides[j].AkahuOnly && strings.EqualFold(settings.Overrides[j].Specific, old.Specific) {
			settings.Overrides[j].Broad, settings.Overrides[j].Specific = next.Broad, next.Specific
		}
	}
	if err := app.renameBudgetItem(old, next); err != nil {
		return err
	}
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, id, "")
}

// renameBudgetItem moves a category's budget target to its new name. A group
// is budgeted under its own label, so its target follows the rename too.
func (app *Application) renameBudgetItem(old, next models.CustomCategory) error {
	if strings.EqualFold(old.Specific, next.Specific) && old.Broad == next.Broad {
		return nil
	}
	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return err
	}
	for _, item := range items {
		if !strings.EqualFold(item.Category, old.Specific) {
			continue
		}
		item.Category = next.Specific
		if item.BroadCategory == old.Broad {
			item.BroadCategory = next.Broad
		}
		if err := app.store.UpdateBudgetItem(item); err != nil {
			return err
		}
	}
	return nil
}

// SettingsMoveCategory moves one of the owner's categories up or down the
// list. Between two rules of the same kind, the one in the earlier category
// wins.
func (app *Application) SettingsMoveCategory(c echo.Context) error {
	id := c.Param("id")
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(settings.Categories, func(c models.CustomCategory) bool { return c.ID == id })
	j := i + 1
	if c.FormValue("direction") == "up" {
		j = i - 1
	}
	if i < 0 || j < 0 || j >= len(settings.Categories) {
		return app.renderCategories(c, id, "")
	}
	settings.Categories[i], settings.Categories[j] = settings.Categories[j], settings.Categories[i]
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, id, "")
}

// SettingsDeleteCategory removes one of the owner's categories with the rules
// filing into it. What they filed goes back to Akahu's category, or to
// another rule. A built in group's category stays, since the group needs it.
func (app *Application) SettingsDeleteCategory(c echo.Context) error {
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	id := c.Param("id")
	i := slices.IndexFunc(settings.Categories, func(c models.CustomCategory) bool { return c.ID == id })
	if i < 0 {
		return app.renderCategories(c, "", "")
	}
	category := settings.Categories[i]
	if builtInGroup(category.SpendGroup) {
		return app.renderCategories(c, id, "A built in group's category cannot be deleted; delete its rules instead.")
	}
	settings.Categories = slices.Delete(settings.Categories, i, i+1)
	settings.Rules = slices.DeleteFunc(settings.Rules, func(r models.CategoryRule) bool {
		return strings.EqualFold(r.Specific, category.Specific)
	})
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, "", "")
}

// ruleFromForm reads the rule being added, edited or previewed. A type rule
// matches on the type alone, so any text typed for it is dropped.
func ruleFromForm(value func(string) string) models.CategoryRule {
	rule := models.CategoryRule{
		Match:            value("match"),
		Contains:         strings.TrimSpace(value("contains")),
		Type:             value("type"),
		IncludeTransfers: value("include_transfers") != "",
	}
	switch rule.Match {
	case models.MatchType:
		rule.Contains = ""
	case models.MatchText, models.MatchMerchant, models.MatchKeyword:
	default:
		rule.Match = models.MatchText
	}
	return rule
}

// ruleProblem says what a rule still needs before it can match anything.
func ruleProblem(rule models.CategoryRule) string {
	switch {
	case rule.Match == models.MatchType && rule.Type == "":
		return "Pick the type of transaction the rule files."
	case rule.Match != models.MatchType && rule.Contains == "":
		return "A rule needs some text to match."
	}
	return ""
}

// blockKey is the key of the block a category is drawn in.
func blockKey(settings models.CategorySettings, specific string) string {
	for _, category := range settings.Categories {
		if strings.EqualFold(category.Specific, specific) {
			return category.ID
		}
	}
	return specific
}

func (app *Application) SettingsAddRule(c echo.Context) error {
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	txs, err := app.store.ReadTransactions()
	if err != nil {
		return err
	}
	target, ok := categoryCatalogue(settings.Categories, txs)[strings.ToLower(c.FormValue("category"))]
	if !ok {
		return app.renderCategories(c, "", "Pick a category for the rule to file transactions under.")
	}
	open := blockKey(settings, target.Specific)
	rule := ruleFromForm(c.FormValue)
	if problem := ruleProblem(rule); problem != "" {
		return app.renderCategories(c, open, problem)
	}

	rule.ID, rule.Broad, rule.Specific = newID(), target.Broad, target.Specific
	settings.Rules = append(settings.Rules, rule)
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, open, "")
}

// SettingsUpdateRule changes what a rule matches, keeping the category it
// files under and its place in it.
func (app *Application) SettingsUpdateRule(c echo.Context) error {
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(settings.Rules, func(r models.CategoryRule) bool { return r.ID == c.Param("id") })
	if i < 0 {
		return echo.NewHTTPError(http.StatusNotFound, "No such rule.")
	}
	open := blockKey(settings, settings.Rules[i].Specific)
	edited := ruleFromForm(c.FormValue)
	if problem := ruleProblem(edited); problem != "" {
		return app.renderCategories(c, open, problem)
	}
	rule := &settings.Rules[i]
	rule.Match, rule.Contains, rule.Type, rule.IncludeTransfers = edited.Match, edited.Contains, edited.Type, edited.IncludeTransfers
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, open, "")
}

func (app *Application) SettingsDeleteRule(c echo.Context) error {
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	id := c.Param("id")
	open := ""
	settings.Rules = slices.DeleteFunc(settings.Rules, func(r models.CategoryRule) bool {
		if r.ID == id {
			open = blockKey(settings, r.Specific)
			return true
		}
		return false
	})
	if err := app.saveCategories(settings); err != nil {
		return err
	}
	return app.renderCategories(c, open, "")
}

// rulePreviewSamples is how many matching transactions the preview lists.
const rulePreviewSamples = 5

// RulePreview shows what a rule would match before it is saved.
type RulePreview struct {
	Ready   bool
	Matches int // transactions the new rule would file
	Taken   int // matching transactions another rule already files
	// Transfers counts outgoing transfers the rule would leave alone because
	// it does not include transfers, so the form can say why a repayment it
	// was typed for is missing.
	Transfers int
	Samples   []RuleSample
}

// RuleSample is one of the transactions a rule preview lists.
type RuleSample struct {
	Date     time.Time
	Title    string
	Category string
	Amount   float64
}

func buildRulePreview(rule models.CategoryRule, txs []models.Transaction, settings models.CategorySettings) RulePreview {
	preview := RulePreview{Ready: true}
	withTransfers := rule
	withTransfers.IncludeTransfers = true
	var matched []models.Transaction
	for _, tx := range txs {
		if !rule.Matches(tx) {
			if withTransfers.Matches(tx) {
				preview.Transfers++
			}
			continue
		}
		// A new rule goes last in its category, so anything an existing rule
		// matches stays put.
		if settings.MatchingRule(tx) >= 0 {
			preview.Taken++
			continue
		}
		preview.Matches++
		matched = append(matched, tx)
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Date.After(matched[j].Date) })
	for i, tx := range matched {
		if i == rulePreviewSamples {
			break
		}
		category := tx.Category.Name
		if category == "" {
			category = tx.Category.Groups.PersonalFinance.Name
		}
		preview.Samples = append(preview.Samples, RuleSample{
			Date: tx.Date, Title: tx.Description, Category: category, Amount: tx.Amount,
		})
	}
	return preview
}

// SettingsPreviewRule answers a rule form as it is typed into.
func (app *Application) SettingsPreviewRule(c echo.Context) error {
	rule := ruleFromForm(c.QueryParam)
	if ruleProblem(rule) != "" {
		return c.Render(http.StatusOK, "settings.rules.preview", RulePreview{})
	}
	settings, err := app.store.GetCategorySettings()
	if err != nil {
		return err
	}
	txs, err := app.store.ReadTransactions()
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "settings.rules.preview", buildRulePreview(rule, txs, settings))
}
