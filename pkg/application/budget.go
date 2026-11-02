package application

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// CategoryTargetRow is one category on the spending targets page.
type CategoryTargetRow struct {
	BroadCategory string
	SubCategory   string
	RowID         string // HTML-safe id, also how an edit names the row to keep open
	ActualWeekly  float64
	// Populated from a saved BudgetItem, if one exists for this pair.
	ItemID    string
	Target    float64
	Frequency string
	// Comparison fields, in the target's own frequency for display.
	ActualDisplay float64
	TargetDisplay float64
	Per           string // "a week", "a month"...
	Percentage    float64
	IsOver        bool
	HasTarget     bool
	NeedsTarget   bool    // has actual spend but no target set
	PctOfIncome   float64 // targetWeekly / netWeekly (fraction)
	TargetWeekly  float64 // weekly-normalised target for internal use
	Kind          string
	IsSpending    bool // false for savings and ignored rows, which carry no target
	WindowWeeks   int  // how many weeks the actual is averaged over
	// SuggestedMonthly is a target to offer a category that has none: its
	// spend over the same 13 weeks the row is measured over, as a monthly
	// amount rounded up to whole dollars. Monthly targets are measured over
	// that window too, so accepting it lands the row on track rather than
	// tripping over a lumpy month.
	SuggestedMonthly float64
	// Generous marks a target set well above what the category spends, and
	// LoweredTarget is what it could come down to, in the target's own
	// frequency. Zero means the category spends nothing to budget for.
	Generous      bool
	LoweredTarget float64
	// Merchants seen in this category, highest spend first. Informational
	// only — they are derived from transactions, never edited.
	Merchants []string
	Open      bool   // the row being edited, drawn expanded
	Section   string // key of the section the row is drawn in
}

// BarWidth is how full the row's progress bar is drawn, capped at full.
func (r CategoryTargetRow) BarWidth() float64 {
	return math.Min(r.Percentage*100, 100)
}

// topMerchants orders merchant names by total spend, highest first, so that
// truncating the list keeps the most significant ones. Ties break by name to
// keep rendering deterministic.
func topMerchants(spend map[string]float64) []string {
	if len(spend) == 0 {
		return nil
	}
	names := make([]string, 0, len(spend))
	for name := range spend {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if spend[names[i]] != spend[names[j]] {
			return spend[names[i]] > spend[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

// uncategorisedGroup collects debits Akahu could not categorise. They are
// still real outgoings, so leaving them out of the setup table while the
// weekly chart counts them makes weeks look over budget for no visible reason.
const uncategorisedGroup = "Uncategorised"

// uncategorisedLabels names the rows inside the uncategorised group. Without
// a category the transaction type is the most telling thing left: loan
// interest, a standing order to an investment fund and cash withdrawals each
// get a row of their own that a target can be set against.
var uncategorisedLabels = map[string]string{
	"LOAN":           "Loan",
	"STANDING ORDER": "Standing orders",
	"PAYMENT":        "Payments",
	"DIRECT DEBIT":   "Direct debits",
	"ATM":            "ATM withdrawals",
	"EFTPOS":         "EFTPOS",
	"DEBIT":          "Card debits",
	"FEE":            "Fees",
}

// spendCategory decides whether a transaction is spending and, if it is,
// which broad and specific category it belongs to. Every budget view goes
// through here so the chart and the setup table always count the same money.
func spendCategory(tx models.Transaction) (broad, specific string, ok bool) {
	if tx.Amount >= 0 || tx.Type == "TRANSFER" {
		return "", "", false
	}
	broad = tx.Category.Groups.PersonalFinance.Name
	if broad == "" {
		label, known := uncategorisedLabels[tx.Type]
		if !known {
			label = "Other"
		}
		return uncategorisedGroup, label, true
	}
	specific = tx.Category.Name
	if specific == "" {
		specific = broad
	}
	return broad, specific, true
}

// spendMerchant names who a transaction was paid to. Uncategorised debits
// rarely carry a merchant, so the cleaned bank description stands in for it.
// Sync names every transfer's merchant "Transfer", which tells two transfers
// apart no better than nothing, so they always use the description.
func spendMerchant(tx models.Transaction) string {
	if tx.IsTransfer() {
		tx.Merchant.Name = ""
	}
	if tx.Merchant.Name != "" {
		return tx.Merchant.Name
	}
	return timelineTitle(BuildTransactionRow(tx))
}

// budgetGroupsBroad heads the budget section holding Budge's own spend
// groups, built in and the owner's, alongside Akahu's broad categories.
const budgetGroupsBroad = "Your groups"

// budgetLine places a transaction in the budget. One a spend group matches is
// budgeted under that group, net of being paid back, so the budget sees the
// food, rent and lunches Akahu never categorised, and each transaction is
// counted once rather than under both a group and its category. Anything else
// goes through spendCategory. name is who was paid, for the merchant tags.
func budgetLine(tx models.Transaction) (broad, specific, name string, amount float64, ok bool) {
	if key, matched := ClassifySpend(tx); matched {
		if group, found := SpendGroupByKey(key); found {
			return budgetGroupsBroad, group.Label, spendMerchant(tx), -tx.Amount, true
		}
	}
	broad, specific, ok = spendCategory(tx)
	if !ok {
		return "", "", "", 0, false
	}
	return broad, specific, spendMerchant(tx), -tx.Amount, true
}

// TargetSection is one part of the spending targets page. Rows are sorted by
// what they need from the owner rather than by Akahu's groups, so settled
// categories stay out of the way until something changes.
type TargetSection struct {
	Key    string
	Title  string
	Rows   []CategoryTargetRow
	Actual float64 // weekly, across the section's rows
	Target float64 // weekly, across the section's rows
	Open   bool
	OOB    bool // drawn as an out-of-band update after an edit
}

// Section keys, in the order the page shows them.
const (
	sectionAttention  = "attention"
	sectionGenerous   = "generous"
	sectionOnTrack    = "on-track"
	sectionSmall      = "small"
	sectionNotCounted = "not-counted"
)

// smallSpendWeekly is the weekly spend under which a category with no target
// is folded away rather than asking for one: a few dollars a week is not what
// tips a budget over.
const smallSpendWeekly = 5.0

// A target counts as generous when spending is under this share of it and at
// least this far below it each week. Both have to hold, so a small yearly
// budget that has not been touched yet is not flagged for a few dollars.
const (
	generousShare     = 0.5
	generousGapWeekly = 20.0
)

// loweredHeadroom is added to what a generous category spends when offering
// a lower target, so a slightly bigger week does not tip it straight over.
const loweredHeadroom = 1.1

// buildTargetSections sorts rows into the page's sections. Needs attention is
// always open; another section opens only when it holds the row being edited,
// so a row that moves section after an edit never disappears from view.
func buildTargetSections(rows []CategoryTargetRow, openRow string) []TargetSection {
	sections := []TargetSection{
		{Key: sectionAttention, Title: "Needs attention", Open: true},
		{Key: sectionGenerous, Title: "Could be lower", Open: true},
		{Key: sectionOnTrack, Title: "On track"},
		{Key: sectionSmall, Title: "Small categories"},
		{Key: sectionNotCounted, Title: "Not counted"},
	}
	index := map[string]int{sectionAttention: 0, sectionGenerous: 1, sectionOnTrack: 2, sectionSmall: 3, sectionNotCounted: 4}
	for _, row := range rows {
		key := sectionOnTrack
		switch {
		case !row.IsSpending:
			key = sectionNotCounted
		case row.IsOver:
			key = sectionAttention
		case row.Generous:
			key = sectionGenerous
		case row.HasTarget:
			key = sectionOnTrack
		case row.ActualWeekly >= smallSpendWeekly:
			key = sectionAttention
		default:
			key = sectionSmall
		}
		section := &sections[index[key]]
		row.Open = row.RowID == openRow
		row.Section = key
		section.Rows = append(section.Rows, row)
		section.Actual += row.ActualWeekly
		section.Target += row.TargetWeekly
		if row.RowID == openRow {
			section.Open = true
		}
	}

	// Overspends first, worst first, then untargeted spend, largest first.
	attention := sections[index[sectionAttention]].Rows
	sort.SliceStable(attention, func(i, j int) bool {
		a, b := attention[i], attention[j]
		if a.IsOver != b.IsOver {
			return a.IsOver
		}
		if a.IsOver {
			return a.ActualWeekly-a.TargetWeekly > b.ActualWeekly-b.TargetWeekly
		}
		return a.ActualWeekly > b.ActualWeekly
	})
	for _, key := range []string{sectionOnTrack, sectionNotCounted} {
		rows := sections[index[key]].Rows
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].BroadCategory != rows[j].BroadCategory {
				return rows[i].BroadCategory < rows[j].BroadCategory
			}
			return rows[i].SubCategory < rows[j].SubCategory
		})
	}
	generous := sections[index[sectionGenerous]].Rows
	sort.SliceStable(generous, func(i, j int) bool {
		return generous[i].TargetWeekly-generous[i].ActualWeekly > generous[j].TargetWeekly-generous[j].ActualWeekly
	})
	small := sections[index[sectionSmall]].Rows
	sort.SliceStable(small, func(i, j int) bool { return small[i].ActualWeekly > small[j].ActualWeekly })

	kept := sections[:0]
	for _, section := range sections {
		if len(section.Rows) > 0 {
			kept = append(kept, section)
		}
	}
	return kept
}

// CategorySetupData is the view-model for the budget.setup partial.
type CategorySetupData struct {
	Tracking             TrackingSummary
	Rows                 []CategoryTargetRow
	Sections             []TargetSection
	OpenRow              string // RowID of the row being edited, kept open
	HasData              bool
	NetWeekly            float64
	TotalTargetWeekly    float64
	SavingsGoal          float64
	SavingsGoalFrequency string
	SavingsGoalWeekly    float64
	Remaining            float64
	MeetsSavingsGoal     bool
	HasSavingsGoal       bool
	// Weekly spend in categories marked as savings, e.g. a standing order to
	// an investment fund. Shown beside the goal rather than counted as spending.
	TrackedSavingsWeekly float64
	OOB                  bool // drawn as an out-of-band update after an edit
}

// BudgetCardsData is the view-model for the summary cards on the income page
// and the allocation bar on the budget page. Both are driven by the same three
// numbers, so they share one calculation.
type BudgetCardsData struct {
	NetWeekly           float64
	TotalWeeklyExpenses float64
	WeeklySavings       float64
	HasDeficit          bool
	PctAllocatedCss     float64 // clamped to 100 for CSS width
	IsOverAllocated     bool
	HasSalary           bool
}

// BudgetSummary is the view-model rendered by GET /budget/summary.
type BudgetSummary struct {
	GrossHourly          float64
	GrossAnnual          float64
	GrossMonthly         float64
	GrossFortnightly     float64
	GrossWeekly          float64
	PAYEHourly           float64
	PAYEAnnual           float64
	PAYEMonthly          float64
	PAYEFortnightly      float64
	PAYEWeekly           float64
	ACCAnnual            float64
	ACCMonthly           float64
	ACCFortnightly       float64
	ACCWeekly            float64
	ACCHourly            float64
	KiwiSaverHourly      float64
	KiwiSaverAnnual      float64
	KiwiSaverMonthly     float64
	KiwiSaverFortnightly float64
	KiwiSaverWeekly      float64
	StudentLoanHourly    float64
	StudentLoanAnnual    float64
	StudentLoanMonthly   float64
	StudentLoanFortnight float64
	StudentLoanWeekly    float64
	NetHourly            float64
	NetAnnual            float64
	NetMonthly           float64
	NetFortnightly       float64
	NetWeekly            float64
}

// Budget shows how spending is tracking against the targets. Each category is
// measured over a window matched to its target's frequency rather than week
// by week, so a fortnightly mortgage or a monthly power bill reads as on
// track instead of spiking in the week it is paid.
func (app *Application) Budget(c echo.Context) error {
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		app.Toast(c, "Error", "Could not load salary config.")
		return c.NoContent(http.StatusInternalServerError)
	}
	setupData, err := app.buildSetupData()
	if err != nil {
		app.Toast(c, "Error", "Could not load category data.")
		return c.NoContent(http.StatusInternalServerError)
	}
	spread, err := app.budgetSpreadView()
	if err != nil {
		app.Toast(c, "Error", "Could not load the monthly bills.")
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.Render(http.StatusOK, "budget", map[string]interface{}{
		"Income":    buildIncomeTile(salary),
		"SetupData": setupData,
		"Spread":    spread,
	})
}

// TrackingSummary is the budget page's headline, totalled across categories,
// each measured over its own window and expressed per week.
type TrackingSummary struct {
	Actual    float64 // spend in categories with a target
	Target    float64
	OverCount int
	// Spend with no target to measure it against, the usual reason a budget
	// does not add up.
	UntargetedWeekly float64
	UntargetedCount  int
	// Targets set well above what their categories spend.
	GenerousCount int
}

// IsOver reports whether spending in targeted categories is over their
// targets in total, allowing the same dollar a week each row is allowed.
func (t TrackingSummary) IsOver() bool {
	return t.Target > 0 && t.Actual > t.Target+overToleranceWeekly
}

// BarWidth is how full the headline bar is drawn, capped at full.
func (t TrackingSummary) BarWidth() float64 {
	if t.Target <= 0 {
		return 0
	}
	return math.Min(t.Actual/t.Target*100, 100)
}

func buildTrackingSummary(rows []CategoryTargetRow) TrackingSummary {
	var summary TrackingSummary
	for _, row := range rows {
		switch {
		case row.HasTarget:
			summary.Actual += row.ActualWeekly
			summary.Target += row.TargetWeekly
			if row.IsOver {
				summary.OverCount++
			}
			if row.Generous {
				summary.GenerousCount++
			}
		case row.NeedsTarget:
			summary.UntargetedWeekly += row.ActualWeekly
			summary.UntargetedCount++
		}
	}
	return summary
}

// BudgetSaveTarget creates or updates a BudgetItem for a category pair and
// returns the re-rendered row.
func (app *Application) BudgetSaveTarget(c echo.Context) error {
	subCat := c.FormValue("sub_category")
	if subCat == "" {
		return c.NoContent(http.StatusBadRequest)
	}
	broadCat := c.FormValue("broad_category")
	target, _ := strconv.ParseFloat(c.FormValue("target_amount"), 64)
	frequency := c.FormValue("target_frequency")
	if frequency == "" {
		frequency = "weekly"
	}

	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return err
	}
	for _, item := range items {
		if strings.EqualFold(item.Category, subCat) {
			item.Amount = target
			item.Frequency = frequency
			item.BroadCategory = broadCat
			if err := app.store.UpdateBudgetItem(item); err != nil {
				return err
			}
			return app.renderTargetRow(c, sanitizeID(subCat))
		}
	}
	newItem := models.BudgetItem{
		ID:            newID(),
		Name:          subCat,
		Amount:        target,
		Frequency:     frequency,
		Category:      subCat,
		BroadCategory: broadCat,
	}
	if err := app.store.CreateBudgetItem(newItem); err != nil {
		return err
	}
	return app.renderTargetRow(c, sanitizeID(subCat))
}

type budgetKindOption struct{ Value, Label string }

// budgetKindOptions are the choices in each row's "Counts as" menu.
var budgetKindOptions = []budgetKindOption{
	{models.BudgetKindSpending, "Spending"},
	{models.BudgetKindSavings, "Savings"},
	{models.BudgetKindIgnore, "Ignore"},
}

// BudgetSaveKind sets whether a category counts as spending, savings or is
// ignored. Moving a row in or out of spending changes its group's totals and
// the savings footer, so the whole table is re-rendered rather than the row.
func (app *Application) BudgetSaveKind(c echo.Context) error {
	subCat := c.FormValue("sub_category")
	kind := c.FormValue("kind")
	switch kind {
	case models.BudgetKindSpending, models.BudgetKindSavings, models.BudgetKindIgnore:
	default:
		return c.NoContent(http.StatusBadRequest)
	}
	if subCat == "" {
		return c.NoContent(http.StatusBadRequest)
	}

	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return err
	}
	saved := false
	for _, item := range items {
		if strings.EqualFold(item.Category, subCat) {
			item.Kind = kind
			if err := app.store.UpdateBudgetItem(item); err != nil {
				return err
			}
			saved = true
			break
		}
	}
	if !saved {
		if err := app.store.CreateBudgetItem(models.BudgetItem{
			ID:            newID(),
			Name:          subCat,
			Category:      subCat,
			BroadCategory: c.FormValue("broad_category"),
			Frequency:     "weekly",
			Kind:          kind,
		}); err != nil {
			return err
		}
	}

	return app.renderTargetRow(c, sanitizeID(subCat))
}

// TargetRowUpdate answers an edit on the targets page: the edited row, and
// the figures elsewhere on the page that it changes.
type TargetRowUpdate struct {
	Row   CategoryTargetRow
	Setup CategorySetupData
}

// renderTargetRow answers an edit with just the edited row, swapped in place.
// Re-rendering the list instead moved rows between sections and reordered
// them under the pointer. The row keeps the section it is drawn in, given as
// "section", even if the edit means it belongs elsewhere; it moves there the
// next time the page loads. Section totals are worked out with it there too,
// so they agree with what is on screen.
func (app *Application) renderTargetRow(c echo.Context, rowID string) error {
	setup, err := app.buildSetupData()
	if err != nil {
		return err
	}
	setup.Sections = pinRow(buildTargetSections(setup.Rows, rowID), rowID, c.FormValue("section"))
	setup.OOB = true
	for i := range setup.Sections {
		setup.Sections[i].OOB = true
	}
	for _, section := range setup.Sections {
		for _, row := range section.Rows {
			if row.RowID == rowID {
				return c.Render(http.StatusOK, "budget.setup.row.update", TargetRowUpdate{Row: row, Setup: setup})
			}
		}
	}
	return echo.NewHTTPError(http.StatusNotFound, "That category is no longer in the budget.")
}

// pinRow moves a row into the section it is drawn in, so totals match the
// page after an edit that would move it. An unknown section leaves it put.
func pinRow(sections []TargetSection, rowID, key string) []TargetSection {
	from, to := -1, -1
	var row CategoryTargetRow
	for i, section := range sections {
		if section.Key == key {
			to = i
		}
		for _, r := range section.Rows {
			if r.RowID == rowID {
				from, row = i, r
			}
		}
	}
	if from < 0 || to < 0 || from == to {
		return sections
	}
	kept := sections[from].Rows[:0:0]
	for _, r := range sections[from].Rows {
		if r.RowID != rowID {
			kept = append(kept, r)
		}
	}
	sections[from].Rows = kept
	sections[from].Actual -= row.ActualWeekly
	sections[from].Target -= row.TargetWeekly
	row.Section = key
	sections[to].Rows = append(sections[to].Rows, row)
	sections[to].Actual += row.ActualWeekly
	sections[to].Target += row.TargetWeekly
	return sections
}

// BudgetCards returns the summary cards (income / expenses / savings).
func (app *Application) BudgetCards(c echo.Context) error {
	data, err := app.buildBudgetCards()
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "budget.cards", data)
}

// BudgetAllocation returns the bar showing how much of take-home pay the
// targets claim.
func (app *Application) BudgetAllocation(c echo.Context) error {
	data, err := app.buildBudgetCards()
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "budget.allocation", data)
}

func (app *Application) buildBudgetCards() (BudgetCardsData, error) {
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		return BudgetCardsData{}, err
	}
	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return BudgetCardsData{}, err
	}
	netWeekly := computeNetWeekly(salary)
	var totalExpenses float64
	for _, it := range items {
		totalExpenses += weeklyTarget(it)
	}
	savings := netWeekly - totalExpenses
	pct := 0.0
	if netWeekly > 0 {
		pct = totalExpenses / netWeekly
	}
	return BudgetCardsData{
		NetWeekly:           netWeekly,
		TotalWeeklyExpenses: totalExpenses,
		WeeklySavings:       savings,
		HasDeficit:          savings < 0,
		PctAllocatedCss:     math.Min(pct*100, 100),
		IsOverAllocated:     savings < 0,
		HasSalary:           salary.Salary > 0,
	}, nil
}

// BudgetSaveSavingsGoal persists the weekly savings goal.
func (app *Application) BudgetSaveSavingsGoal(c echo.Context) error {
	goal, _ := strconv.ParseFloat(c.FormValue("savings_goal"), 64)
	freq := c.FormValue("savings_goal_frequency")
	if freq == "" {
		freq = "weekly"
	}
	existing, err := app.store.GetBudgetSalary()
	if err != nil {
		return err
	}
	existing.SavingsGoal = goal
	existing.SavingsGoalFrequency = freq
	return app.store.SaveBudgetSalary(existing)
}

func buildSetupRowData(item models.BudgetItem, actualWeekly, netWeekly float64) CategoryTargetRow {
	targetWeekly := weeklyTarget(item)
	isSpending := item.IsSpending()
	hasTarget := targetWeekly > 0

	freq := item.Frequency
	if freq == "" {
		freq = "weekly"
	}

	var pct, pctOfIncome float64
	if hasTarget {
		pct = actualWeekly / targetWeekly
	}
	if netWeekly > 0 {
		pctOfIncome = targetWeekly / netWeekly
	}
	return CategoryTargetRow{
		BroadCategory: item.BroadCategory,
		SubCategory:   item.Category,
		RowID:         sanitizeID(item.Category),
		ActualWeekly:  actualWeekly,
		ItemID:        item.ID,
		Target:        item.Amount,
		Frequency:     item.Frequency,
		ActualDisplay: actualInFrequency(actualWeekly, freq),
		TargetDisplay: item.Amount,
		Per:           frequencyPer(freq),
		WindowWeeks:   actualWindowWeeks(item.Frequency),
		TargetWeekly:  targetWeekly,
		Percentage:    pct,
		PctOfIncome:   pctOfIncome,
		IsOver:        hasTarget && actualWeekly > targetWeekly+overToleranceWeekly,
		HasTarget:     hasTarget,
		NeedsTarget:   isSpending && !hasTarget && actualWeekly > 0,
		Kind:          item.Kind,
		IsSpending:    isSpending,
	}
}

// actualWindowWeeks is how far back a category's actual spend is averaged,
// matched to its target's frequency so weekly categories reflect recent spend
// while yearly ones amortise their bills.
func actualWindowWeeks(frequency string) int {
	switch frequency {
	case "fortnightly":
		return 8
	case "monthly":
		return 13
	case "yearly":
		return 52
	}
	return 4
}

// untargetedWindowWeeks is how far back a category with no target is measured,
// and so what a suggested target is based on. Long enough that one unusual
// month does not set the figure.
const untargetedWindowWeeks = 13

// overToleranceWeekly is how far past its target spend must go to count as
// over, so a target set to the dollar is not flagged for a few cents.
const overToleranceWeekly = 1.0

// markGenerous flags a target set well above what its category spends, and
// works out what it could be lowered to. Spending is judged on the higher of
// the row's own window and the last 13 weeks, so a quiet month does not flag
// a weekly target and a yearly one is not judged on its quiet weeks alone.
func markGenerous(row *CategoryTargetRow, recentWeekly float64) {
	if !row.HasTarget || !row.IsSpending {
		return
	}
	spent := math.Max(row.ActualWeekly, recentWeekly)
	if spent >= row.TargetWeekly*generousShare || row.TargetWeekly-spent < generousGapWeekly {
		return
	}
	row.Generous = true
	freq := row.Frequency
	if freq == "" {
		freq = "weekly"
	}
	row.LoweredTarget = math.Ceil(actualInFrequency(spent*loweredHeadroom, freq))
}

// buildSetupData groups 13 months of transactions by category pair and merges
// in any saved targets, one row per pair.
func (app *Application) buildSetupData() (CategorySetupData, error) {
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		return CategorySetupData{}, err
	}
	netWeekly := computeNetWeekly(salary)

	txs, err := app.store.ReadTransactionsByDate(time.Now().AddDate(0, -13, 0), time.Now())
	if err != nil {
		return CategorySetupData{}, err
	}

	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return CategorySetupData{}, err
	}
	itemBySubCat := budgetItemsByCategory(items)
	spreader, err := app.budgetSpreader()
	if err != nil {
		return CategorySetupData{}, err
	}

	type pairKey struct{ broad, specific string }
	var pairOrder []pairKey
	pairSeen := make(map[pairKey]bool)
	// spend[key][weeks] is the total within that many weeks back; every window
	// a transaction falls inside counts it, so each is a plain total.
	spend := make(map[pairKey]map[int]float64)
	merchantSpend := make(map[pairKey]map[string]float64)
	windows := []int{4, 8, 13, 52}
	now := time.Now()
	weekStart := isoWeekStart(now)

	for _, tx := range txs {
		broad, specific, name, amt, ok := budgetLine(tx)
		if !ok {
			continue
		}
		key := pairKey{broad, specific}
		if !pairSeen[key] {
			pairSeen[key] = true
			pairOrder = append(pairOrder, key)
			spend[key] = make(map[int]float64)
			merchantSpend[key] = make(map[string]float64)
		}
		if name != "" {
			merchantSpend[key][name] += amt
		}
		// A monthly bill counts only the part of its month inside a window.
		for _, weeks := range windows {
			spend[key][weeks] += spreader.share(tx, amt, weekStart.AddDate(0, 0, -7*weeks), now)
		}
	}

	var rows []CategoryTargetRow
	for _, key := range pairOrder {
		saved := itemBySubCat[strings.ToLower(key.specific)]
		weeks := actualWindowWeeks(saved.Frequency)
		if weeklyTarget(saved) == 0 {
			weeks = untargetedWindowWeeks
		}
		row := buildSetupRowData(saved, spend[key][weeks]/float64(weeks), netWeekly)
		row.WindowWeeks = weeks
		if row.BroadCategory == "" {
			row.BroadCategory = key.broad
		}
		if row.SubCategory == "" {
			row.SubCategory = key.specific
			row.RowID = sanitizeID(key.specific)
		}
		if !row.HasTarget && row.IsSpending {
			row.SuggestedMonthly = math.Ceil(row.ActualWeekly * 52 / 12)
		}
		if row.HasTarget && !row.IsOver {
			markGenerous(&row, spend[key][untargetedWindowWeeks]/untargetedWindowWeeks)
		}
		row.Merchants = topMerchants(merchantSpend[key])
		rows = append(rows, row)
	}

	// A saved category with no spending in the window still has a target
	// that counts toward the total, so it gets a row too; otherwise it could
	// be neither seen nor changed.
	shown := make(map[string]bool, len(rows))
	for _, row := range rows {
		shown[strings.ToLower(row.SubCategory)] = true
	}
	for _, item := range items {
		if shown[strings.ToLower(item.Category)] {
			continue
		}
		row := buildSetupRowData(item, 0, netWeekly)
		if weeklyTarget(item) == 0 {
			row.WindowWeeks = untargetedWindowWeeks
		}
		markGenerous(&row, 0)
		rows = append(rows, row)
	}

	var totalTargetWeekly, trackedSavings float64
	for _, row := range rows {
		totalTargetWeekly += row.TargetWeekly
		if row.Kind == models.BudgetKindSavings {
			trackedSavings += row.ActualWeekly
		}
	}

	savingsGoalWeekly := toWeeklyAmount(salary.SavingsGoal, salary.SavingsGoalFrequency)
	remaining := netWeekly - totalTargetWeekly

	return CategorySetupData{
		Tracking:             buildTrackingSummary(rows),
		Rows:                 rows,
		Sections:             buildTargetSections(rows, ""),
		HasData:              len(txs) > 0,
		NetWeekly:            netWeekly,
		TotalTargetWeekly:    totalTargetWeekly,
		SavingsGoal:          salary.SavingsGoal,
		SavingsGoalFrequency: salary.SavingsGoalFrequency,
		SavingsGoalWeekly:    savingsGoalWeekly,
		Remaining:            remaining,
		MeetsSavingsGoal:     savingsGoalWeekly > 0 && remaining >= savingsGoalWeekly,
		HasSavingsGoal:       salary.SavingsGoal > 0,
		TrackedSavingsWeekly: trackedSavings,
	}, nil
}

// BudgetSaveSalary persists salary config (called on field change, hx-swap="none").
func (app *Application) BudgetSaveSalary(c echo.Context) error {
	salary, _ := strconv.ParseFloat(c.FormValue("salary"), 64)
	kiwiSaverRate, _ := strconv.ParseFloat(c.FormValue("kiwisaver_rate"), 64)
	existing, err := app.store.GetBudgetSalary()
	if err != nil {
		return err
	}
	// Mutate in place rather than rebuilding the struct, so fields owned by
	// other forms (savings goal, categories) survive an income edit.
	existing.Salary = salary
	existing.SalaryFrequency = c.FormValue("salary_frequency")
	existing.IncludePAYE = c.FormValue("include_paye") == "on"
	existing.KiwiSaverRate = kiwiSaverRate
	existing.StudentLoan = c.FormValue("student_loan") == "on"
	return app.store.SaveBudgetSalary(existing)
}

// BudgetSummary reads saved state and returns calculated summary HTML.
func (app *Application) BudgetSummary(c echo.Context) error {
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		return err
	}
	return c.Render(http.StatusOK, "budget.summary", buildBudgetSummary(salary))
}

// IncomeTileData is the view-model for the income summary on the budget page.
type IncomeTileData struct {
	Salary     models.BudgetSalary
	Summary    BudgetSummary
	Deductions []string // the deductions that apply, named for the tile
}

func buildIncomeTile(salary models.BudgetSalary) IncomeTileData {
	tile := IncomeTileData{Salary: salary, Summary: buildBudgetSummary(salary)}
	if salary.IncludePAYE {
		tile.Deductions = append(tile.Deductions, "PAYE", "ACC")
	}
	if salary.KiwiSaverRate > 0 {
		tile.Deductions = append(tile.Deductions, fmt.Sprintf("KiwiSaver %g%%", salary.KiwiSaverRate))
	}
	if salary.StudentLoan {
		tile.Deductions = append(tile.Deductions, "Student loan")
	}
	return tile
}

// BudgetIncome is the page for configuring income, kept off the budget page
// because it is set once and rarely revisited.
func (app *Application) BudgetIncome(c echo.Context) error {
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		app.Toast(c, "Error", "Could not load salary config.")
		return c.NoContent(http.StatusInternalServerError)
	}
	return c.Render(http.StatusOK, "budget.income", map[string]interface{}{"Salary": salary})
}

func buildBudgetSummary(salary models.BudgetSalary) BudgetSummary {
	annual := toAnnual(salary.Salary, salary.SalaryFrequency)
	var payeAnnual, accAnnual, studentLoanAnnual float64
	if salary.IncludePAYE {
		payeAnnual = calculateNZPAYE(annual)
		accAnnual = calculateNZACC(annual)
	}
	if salary.StudentLoan {
		studentLoanAnnual = calculateNZStudentLoan(annual)
	}
	kiwiSaverAnnual := annual * (salary.KiwiSaverRate / 100)
	netAnnual := annual - payeAnnual - accAnnual - kiwiSaverAnnual - studentLoanAnnual

	return BudgetSummary{
		GrossHourly:          annual / 52 / 40,
		GrossAnnual:          annual,
		GrossMonthly:         annual / 12,
		GrossFortnightly:     annual / 26,
		GrossWeekly:          annual / 52,
		PAYEHourly:           payeAnnual / 52 / 40,
		PAYEAnnual:           payeAnnual,
		PAYEMonthly:          payeAnnual / 12,
		PAYEFortnightly:      payeAnnual / 26,
		PAYEWeekly:           payeAnnual / 52,
		ACCHourly:            accAnnual / 52 / 40,
		ACCAnnual:            accAnnual,
		ACCMonthly:           accAnnual / 12,
		ACCFortnightly:       accAnnual / 26,
		ACCWeekly:            accAnnual / 52,
		KiwiSaverHourly:      kiwiSaverAnnual / 52 / 40,
		KiwiSaverAnnual:      kiwiSaverAnnual,
		KiwiSaverMonthly:     kiwiSaverAnnual / 12,
		KiwiSaverFortnightly: kiwiSaverAnnual / 26,
		KiwiSaverWeekly:      kiwiSaverAnnual / 52,
		StudentLoanHourly:    studentLoanAnnual / 52 / 40,
		StudentLoanAnnual:    studentLoanAnnual,
		StudentLoanMonthly:   studentLoanAnnual / 12,
		StudentLoanFortnight: studentLoanAnnual / 26,
		StudentLoanWeekly:    studentLoanAnnual / 52,
		NetHourly:            netAnnual / 52 / 40,
		NetAnnual:            netAnnual,
		NetMonthly:           netAnnual / 12,
		NetFortnightly:       netAnnual / 26,
		NetWeekly:            netAnnual / 52,
	}
}

// budgetItemsByCategory indexes items by their lower-cased subcategory, the
// key every budget view matches transactions on.
func budgetItemsByCategory(items []models.BudgetItem) map[string]models.BudgetItem {
	byCategory := make(map[string]models.BudgetItem, len(items))
	for _, item := range items {
		byCategory[strings.ToLower(item.Category)] = item
	}
	return byCategory
}

// countedSpend is spendCategory narrowed to what counts against the budget:
// categories marked as savings or ignored are left out. A category with no
// saved item is spending.
func countedSpend(tx models.Transaction, items map[string]models.BudgetItem) (broad, specific string, ok bool) {
	broad, specific, _, _, ok = budgetLine(tx)
	if !ok {
		return "", "", false
	}
	if item, saved := items[strings.ToLower(specific)]; saved && !item.IsSpending() {
		return "", "", false
	}
	return broad, specific, true
}

// withoutUncounted drops debits in categories the owner has marked as savings
// or ignored on the targets page, so every view of spending agrees with the
// budget: a contribution to an investment fund is not spending, and loan
// interest is not counted again on top of the repayments that pay it.
func (app *Application) withoutUncounted(txs []models.Transaction) ([]models.Transaction, error) {
	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return nil, err
	}
	byCategory := budgetItemsByCategory(items)
	kept := make([]models.Transaction, 0, len(txs))
	for _, tx := range txs {
		_, _, _, _, spend := budgetLine(tx)
		_, _, counted := countedSpend(tx, byCategory)
		if !spend || counted {
			kept = append(kept, tx)
		}
	}
	return kept, nil
}

// isoWeekStart returns midnight on the Monday that starts t's week, in
// local time. Weeks are local because that is where Monday is: in UTC a New
// Zealand week started at 1pm on Monday, so a Monday morning purchase landed
// in the week before. Transaction dates are read in local time before they
// are bucketed, so keys from either side compare equal.
func isoWeekStart(t time.Time) time.Time {
	t = t.In(time.Local)
	y, m, d := t.Date()
	t0 := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	wd := int(t0.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t0.AddDate(0, 0, -(wd - 1))
}

// ---- pure functions ----

// weeklyTarget returns the weekly-normalised budget target for an item.
// Savings and ignored items are not spending, so they contribute nothing
// even if an amount was saved before the switch.
func weeklyTarget(item models.BudgetItem) float64 {
	if !item.IsSpending() {
		return 0
	}
	return toWeeklyAmount(item.Amount, item.Frequency)
}

func actualInFrequency(weeklyActual float64, frequency string) float64 {
	switch frequency {
	case "weekly", "":
		return weeklyActual
	case "fortnightly":
		return weeklyActual * 2
	case "monthly":
		return weeklyActual * 52 / 12
	case "yearly":
		return weeklyActual * 52
	}
	return weeklyActual
}

// frequencyPer reads a target's frequency the way the targets page phrases
// it after an amount: "$150 a week".
func frequencyPer(frequency string) string {
	switch frequency {
	case "fortnightly":
		return "a fortnight"
	case "monthly":
		return "a month"
	case "yearly":
		return "a year"
	}
	return "a week"
}

// sanitizeID converts a string to a safe HTML id, prefixed with "sc-" (subcategory rows).
func sanitizeID(s string) string { return sanitizeWithPrefix("sc-", s) }

func sanitizeWithPrefix(prefix, s string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// computeNetWeekly returns weekly take-home pay after all deductions.
func computeNetWeekly(salary models.BudgetSalary) float64 {
	annual := toAnnual(salary.Salary, salary.SalaryFrequency)
	var payeAnnual, accAnnual, slAnnual float64
	if salary.IncludePAYE {
		payeAnnual = calculateNZPAYE(annual)
		accAnnual = calculateNZACC(annual)
	}
	if salary.StudentLoan {
		slAnnual = calculateNZStudentLoan(annual)
	}
	return (annual - payeAnnual - accAnnual - annual*(salary.KiwiSaverRate/100) - slAnnual) / 52
}

func toAnnual(amount float64, frequency string) float64 {
	switch frequency {
	case "yearly":
		return amount
	case "monthly":
		return amount * 12
	case "fortnightly":
		return amount * 26
	}
	return amount
}

// calculateNZPAYE computes annual PAYE using NZ brackets updated 31 July 2024.
func calculateNZPAYE(annual float64) float64 {
	type bracket struct{ max, rate float64 }
	brackets := []bracket{
		{15600, 0.105},
		{53500, 0.175},
		{78100, 0.300},
		{180000, 0.330},
		{math.MaxFloat64, 0.390},
	}
	tax, prev := 0.0, 0.0
	for _, b := range brackets {
		if annual <= prev {
			break
		}
		tax += (math.Min(annual, b.max) - prev) * b.rate
		prev = b.max
	}
	return tax
}

// calculateNZACC computes the annual ACC earners' levy.
// Current rate: 1.75% on earnings up to $142,283 (2025–26).
// Rises to 1.83% on earnings up to $160,244 from 1 April 2027.
func calculateNZACC(annual float64) float64 {
	const rate, maxEarnings = 0.0175, 142283.0
	return math.Min(annual, maxEarnings) * rate
}

// calculateNZStudentLoan computes the annual student loan repayment (12% above threshold).
func calculateNZStudentLoan(annual float64) float64 {
	const rate, threshold = 0.12, 22828.0
	if annual <= threshold {
		return 0
	}
	return (annual - threshold) * rate
}

func toWeeklyAmount(amount float64, frequency string) float64 {
	switch frequency {
	case "weekly":
		return amount
	case "fortnightly":
		return amount / 2
	case "monthly":
		return amount * 12 / 52
	case "yearly":
		return amount / 52
	}
	return amount
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ---- performance chart ----

// BudgetPerformanceData is the view-model for the budget.performance partial.
// Labels and Actuals are JSON strings embedded as data attributes on the canvas;
// Go's HTML-encoding of attribute values keeps them safe, and dataset decodes them.
type BudgetPerformanceData struct {
	Labels  string
	Actuals string
	Target  float64
	HasData bool

	// Net and Savings let the chart split each period's take-home into what
	// was spent, what was left for the savings goal, and the rest. Take-home
	// is the budgeted figure rather than when pay landed: paid monthly, three
	// weeks in four would show no income at all. Target, Net and Savings are
	// all per period of the chart, a week or a month.
	Net     float64
	Savings float64

	// MoneyIn is what arrived in each period, as JSON. Weeks all carry the
	// average week, since pay lands monthly.
	MoneyIn string

	Range  PerformanceRange
	Ranges []PerformanceRange
}

// moneyIn is what a transaction brought in from outside the owner's tracked
// accounts: pay, people paying them, refunds, and money moved in from an
// account they have switched off. Moving money between tracked accounts
// brings nothing in, and money a spend group already nets off its spending,
// such as being paid back for groceries, is not counted a second time.
// external reports whether a paired transfer's other leg is in an account
// Budge no longer tracks; such a transfer counts both ways, so money sent
// back to that account takes from what came in.
func moneyIn(tx models.Transaction, external bool) float64 {
	if _, ok := ClassifySpend(tx); ok {
		return 0
	}
	if tx.Category.Name == "Transfer" {
		if external {
			return tx.Amount
		}
		return 0
	}
	if tx.Amount > 0 {
		return tx.Amount
	}
	return 0
}

// externalTransfers finds the paired transfers whose other leg is missing:
// the sync paired them when both accounts were tracked, and the other has
// since been switched off, its transactions with it. From here on they are
// money arriving from, or leaving to, outside.
func externalTransfers(txs []models.Transaction) map[string]bool {
	type leg struct {
		account string
		date    time.Time
	}
	legs := map[int64][]leg{}
	for _, tx := range txs {
		if tx.Category.Name == "Transfer" {
			cents := int64(math.Round(tx.Amount * 100))
			legs[cents] = append(legs[cents], leg{tx.Account, tx.Date})
		}
	}
	external := map[string]bool{}
	for _, tx := range txs {
		if tx.Category.Name != "Transfer" {
			continue
		}
		matched := false
		for _, other := range legs[-int64(math.Round(tx.Amount*100))] {
			if other.account != tx.Account && math.Abs(other.date.Sub(tx.Date).Hours()) <= 24 {
				matched = true
				break
			}
		}
		if !matched {
			external[tx.Id] = true
		}
	}
	return external
}

// PerformanceRange is a span the budget chart offers: so many weeks, or so
// many calendar months.
type PerformanceRange struct {
	Key     string
	Label   string
	Periods int
	Monthly bool
}

var performanceRanges = []PerformanceRange{
	{"w8", "Last 8 weeks", 8, false},
	{"w13", "Last 13 weeks", 13, false},
	{"w26", "Last 26 weeks", 26, false},
	{"w52", "Last 52 weeks", 52, false},
	{"m6", "Last 6 months", 6, true},
	{"m12", "Last 12 months", 12, true},
}

// weeksPerMonth turns weekly figures into monthly ones.
const weeksPerMonth = 52.0 / 12.0

func (app *Application) BudgetPerformance(c echo.Context) error {
	span := performanceRanges[1]
	for _, r := range performanceRanges {
		if r.Key == c.QueryParam("range") {
			span = r
		}
	}

	// Buckets oldest first, the period in progress last. Weeks start in UTC,
	// as the transaction dates are stored; months run payday to payday when
	// pay can be found, as everywhere spending is counted by month.
	now := time.Now()
	cadence := CadenceWeekly
	bucket := isoWeekStart
	if span.Monthly {
		cadence = CadenceMonthly
		bucket = func(t time.Time) time.Time { return spendPeriodStart(t.In(now.Location()), CadenceMonthly) }
	}
	starts := make([]time.Time, span.Periods)
	for i := range starts {
		if span.Monthly {
			starts[i] = spendPeriodAdd(bucket(now), cadence, -(span.Periods - 1 - i))
		} else {
			starts[span.Periods-1-i] = isoWeekStart(now).AddDate(0, 0, -7*i)
		}
	}

	// A monthly bill paid before the first period still covers part of it.
	txs, err := app.store.ReadTransactionsByDate(budgetSpreadLookback(starts[0]), time.Now())
	if err != nil {
		return err
	}
	spreader, err := app.budgetSpreader()
	if err != nil {
		return err
	}
	items, err := app.store.ReadBudgetItems()
	if err != nil {
		return err
	}
	salary, err := app.store.GetBudgetSalary()
	if err != nil {
		return err
	}

	// Targets, take-home and the savings goal are kept weekly, and scaled to
	// the period the chart is drawn in.
	scale := 1.0
	if span.Monthly {
		scale = weeksPerMonth
	}
	var target float64
	for _, item := range items {
		target += weeklyTarget(item)
	}

	// A loan account's own entries are not spending: the drawdown is the
	// lender paying out, and its interest is part of the repayments, which
	// are already counted leaving the everyday account. Counting both put
	// a whole loan into the month it was taken out.
	accounts, err := app.store.ReadAccounts()
	if err != nil {
		return err
	}
	loans := map[string]bool{}
	for _, account := range accounts {
		if account.Type == "LOAN" {
			loans[account.Id] = true
		}
	}

	// Every period's spending, net of what spend groups were paid back, so
	// the chart agrees with the rows above it, and what came in.
	totals := make(map[time.Time]float64)
	incoming := make(map[time.Time]float64)
	external := externalTransfers(txs)
	byCategory := budgetItemsByCategory(items)
	periodEnd := func(start time.Time) time.Time {
		if span.Monthly {
			return spendPeriodAdd(start, cadence, 1)
		}
		return start.AddDate(0, 0, 7)
	}
	for _, tx := range txs {
		if loans[tx.Account] {
			continue
		}
		incoming[bucket(tx.Date)] += moneyIn(tx, external[tx.Id])
		_, specific, _, amt, ok := budgetLine(tx)
		if !ok {
			continue
		}
		if item, saved := byCategory[strings.ToLower(specific)]; saved && !item.IsSpending() {
			continue
		}
		// A monthly bill is shared across the periods its month covers;
		// anything else lands whole in its own.
		if !spreader.monthly(tx) {
			totals[bucket(tx.Date)] += amt
			continue
		}
		for _, start := range starts {
			totals[start] += spreader.share(tx, amt, start, periodEnd(start))
		}
	}

	labels := make([]string, span.Periods)
	actuals := make([]float64, span.Periods)
	ins := make([]float64, span.Periods)
	for i, start := range starts {
		switch {
		case span.Monthly && payCycleKnown():
			labels[i] = start.Format("2 Jan") + " – " + spendPeriodAdd(start, cadence, 1).AddDate(0, 0, -1).Format("2 Jan")
		case span.Monthly:
			labels[i] = start.Format("Jan 2006")
		default:
			labels[i] = start.Format("2 Jan")
		}
		actuals[i] = math.Round(totals[start]*100) / 100
		ins[i] = math.Round(incoming[start]*100) / 100
	}

	// Pay lands once a month, so a week's money in is one huge week and three
	// empty ones. Weeks are measured against the average week instead, taken
	// over the completed weeks shown.
	if !span.Monthly && span.Periods > 1 {
		var sum float64
		for _, in := range ins[:span.Periods-1] {
			sum += in
		}
		average := math.Round(sum/float64(span.Periods-1)*100) / 100
		for i := range ins {
			ins[i] = average
		}
	}

	labelsJSON, _ := json.Marshal(labels)
	actualsJSON, _ := json.Marshal(actuals)
	insJSON, _ := json.Marshal(ins)
	round := func(v float64) float64 { return math.Round(v*scale*100) / 100 }

	return c.Render(http.StatusOK, "budget.performance", BudgetPerformanceData{
		Labels:  string(labelsJSON),
		Actuals: string(actualsJSON),
		Target:  round(target),
		HasData: len(txs) > 0 || target > 0,
		MoneyIn: string(insJSON),
		Range:   span,
		Ranges:  performanceRanges,
		Net:     round(computeNetWeekly(salary)),
		Savings: round(toWeeklyAmount(salary.SavingsGoal, salary.SavingsGoalFrequency)),
	})
}
