package application

import (
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// budgetSpreader spreads monthly payments over the month they cover. Paid
// on the day it leaves, a $200 power bill is a blown week and three quiet
// ones; spread, it is about $46 a week, which is what it actually costs. A
// payment is monthly when the owner named it, or when it is one of a run of
// payments to the same payee about a month apart.
type budgetSpreader struct {
	names    []string
	detected map[string]bool

	// Detected names the payees found recurring monthly, for the page.
	Detected []string
}

func newBudgetSpreader(spread models.BudgetSpread, history []models.Transaction) budgetSpreader {
	s := budgetSpreader{names: ruleNames(spread.Monthly)}
	ignore := ruleNames(spread.Ignore)
	s.detected, s.Detected = detectMonthly(history, func(tx models.Transaction) bool {
		return matchesName(ignore, tx) || matchesName(s.names, tx)
	})
	return s
}

// ruleNames squashes typed names for matching, dropping any left empty.
func ruleNames(list []string) []string {
	var names []string
	for _, name := range list {
		if n := lettersOnly(name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// matchesName reports whether a payment's merchant name or description holds
// one of the names, ignoring case, spaces and punctuation, the way a group's
// merchants match.
func matchesName(names []string, tx models.Transaction) bool {
	candidates := []string{lettersOnly(tx.Merchant.Name), lettersOnly(tx.Description)}
	for _, name := range names {
		for _, candidate := range candidates {
			if candidate != "" && strings.Contains(candidate, name) {
				return true
			}
		}
	}
	return false
}

// monthly reports whether a payment is one to spread.
func (s budgetSpreader) monthly(tx models.Transaction) bool {
	return s.detected[tx.Id] || matchesName(s.names, tx)
}

// A run of payments counts as monthly once it has monthlyRunLength payments,
// each monthlyGapMin to monthlyGapMax days after the last and no more than
// monthlyDrift times larger or smaller than it: a power bill moves with the
// seasons, but not from $20 to $200.
const (
	monthlyRunLength = 3
	monthlyGapMin    = 26
	monthlyGapMax    = 35
	monthlyDrift     = 1.5
)

// detectMonthly finds the payments that recur about once a month to the same
// payee. Payees are told apart by the letters of their name alone, so a
// description carrying this month's reference number still matches last
// month's. skip leaves out payments the owner has already decided on.
// Moving money between the owner's own accounts is never a bill.
func detectMonthly(history []models.Transaction, skip func(models.Transaction) bool) (map[string]bool, []string) {
	type payment struct {
		tx   models.Transaction
		name string
	}
	byPayee := make(map[string][]payment)
	for _, tx := range history {
		if tx.Amount >= 0 || tx.Category.Name == "Transfer" || tx.Id == "" || skip(tx) {
			continue
		}
		name := tx.Merchant.Name
		if name == "" {
			name = tx.Description
		}
		key := payeeKey(name)
		if key == "" {
			continue
		}
		byPayee[key] = append(byPayee[key], payment{tx, name})
	}

	detected := make(map[string]bool)
	var names []string
	for _, payments := range byPayee {
		slices.SortFunc(payments, func(a, b payment) int { return a.tx.Date.Compare(b.tx.Date) })
		found := ""
		run := []payment{payments[0]}
		flush := func() {
			if len(run) >= monthlyRunLength {
				for _, p := range run {
					detected[p.tx.Id] = true
				}
				found = payeeLabel(run[len(run)-1].name)
			}
		}
		for _, p := range payments[1:] {
			prev := run[len(run)-1]
			days := p.tx.Date.Sub(prev.tx.Date).Hours() / 24
			ratio := p.tx.Amount / prev.tx.Amount
			if days >= monthlyGapMin && days <= monthlyGapMax && ratio <= monthlyDrift && ratio >= 1/monthlyDrift {
				run = append(run, p)
				continue
			}
			flush()
			run = []payment{p}
		}
		flush()
		if found != "" {
			names = append(names, found)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return detected, names
}

// payeeLabel names a payee for the page, without the reference numbers a
// bank adds to a description.
func payeeLabel(name string) string {
	var words []string
	for _, word := range strings.Fields(name) {
		if !strings.ContainsFunc(word, unicode.IsDigit) {
			words = append(words, word)
		}
	}
	if len(words) == 0 {
		return name
	}
	return strings.Join(words, " ")
}

// payeeKey is the letters of a payee's name, lower cased, so the numbers a
// bank adds to a description do not split one payee into many.
func payeeKey(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
}

// share is how much of amount, paid by tx, falls in [from, to). A monthly
// payment covers the month from the day it was paid, evenly by the hour; any
// other payment falls whole on its date.
func (s budgetSpreader) share(tx models.Transaction, amount float64, from, to time.Time) float64 {
	if !s.monthly(tx) {
		if tx.Date.Before(from) || !tx.Date.Before(to) {
			return 0
		}
		return amount
	}
	start := startOfDay(tx.Date, time.Local)
	end := start.AddDate(0, 1, 0)
	overlap := minTime(end, to).Sub(maxTime(start, from))
	if overlap <= 0 {
		return 0
	}
	return amount * overlap.Hours() / end.Sub(start).Hours()
}

// budgetSpreadLookback is how far before a window a monthly payment can
// still reach into it.
func budgetSpreadLookback(from time.Time) time.Time {
	return from.AddDate(0, -1, -1)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// budgetSpreadHistory is how far back monthly payments are looked for.
const budgetSpreadHistory = 13

func (app *Application) budgetSpreader() (budgetSpreader, error) {
	spread, err := app.store.GetBudgetSpread()
	if err != nil {
		return budgetSpreader{}, err
	}
	history, err := app.store.ReadTransactionsByDate(time.Now().AddDate(0, -budgetSpreadHistory, 0), time.Now())
	if err != nil {
		return budgetSpreader{}, err
	}
	return newBudgetSpreader(spread, history), nil
}

// BudgetSpreadView is the monthly bills card: what the owner named, and
// what was found without being named.
type BudgetSpreadView struct {
	Spread   models.BudgetSpread
	Detected []string
}

func (app *Application) budgetSpreadView() (BudgetSpreadView, error) {
	spread, err := app.store.GetBudgetSpread()
	if err != nil {
		return BudgetSpreadView{}, err
	}
	spreader, err := app.budgetSpreader()
	if err != nil {
		return BudgetSpreadView{}, err
	}
	return BudgetSpreadView{Spread: spread, Detected: spreader.Detected}, nil
}

// BudgetSaveSpread stores the payments to spread over a month, then has the
// budget redraw so the chart shows the difference at once.
func (app *Application) BudgetSaveSpread(c echo.Context) error {
	spread := models.BudgetSpread{
		Monthly: parseRuleList(c.FormValue("monthly")),
		Ignore:  parseRuleList(c.FormValue("ignore")),
	}
	if err := app.store.SaveBudgetSpread(spread); err != nil {
		app.Toast(c, "Error", "Could not save the monthly bills.")
		return c.NoContent(http.StatusInternalServerError)
	}
	setup, err := app.buildSetupData()
	if err != nil {
		app.Toast(c, "Error", "Could not load category data.")
		return c.NoContent(http.StatusInternalServerError)
	}
	view, err := app.budgetSpreadView()
	if err != nil {
		app.Toast(c, "Error", "Could not load the monthly bills.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Saved", "Monthly bills saved.")
	// Toast already uses HX-Trigger, so the chart's refresh rides on its own
	// header rather than overwriting the toast.
	c.Response().Header().Set("HX-Trigger-After-Swap", "refreshSummary")
	return c.Render(http.StatusOK, "budget.spread.saved", map[string]interface{}{
		"Spread":    view,
		"SetupData": setup,
	})
}
