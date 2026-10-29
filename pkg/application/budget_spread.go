package application

import (
	"net/http"
	"strings"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// budgetSpreader spreads the payments the owner named as monthly over the
// month they cover. Paid on the day it leaves, a $200 power bill is a blown
// week and three quiet ones; spread, it is about $46 a week, which is what
// it actually costs.
type budgetSpreader struct {
	names []string
}

func newBudgetSpreader(spread models.BudgetSpread) budgetSpreader {
	var names []string
	for _, name := range spread.Monthly {
		if n := lettersOnly(name); n != "" {
			names = append(names, n)
		}
	}
	return budgetSpreader{names: names}
}

// monthly reports whether a payment is one to spread. Like a group's
// merchants, an entry matches the merchant name or the description,
// ignoring case, spaces and punctuation.
func (s budgetSpreader) monthly(tx models.Transaction) bool {
	if len(s.names) == 0 {
		return false
	}
	candidates := []string{lettersOnly(tx.Merchant.Name), lettersOnly(tx.Description)}
	for _, name := range s.names {
		for _, candidate := range candidates {
			if candidate != "" && strings.Contains(candidate, name) {
				return true
			}
		}
	}
	return false
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

func (app *Application) budgetSpreader() (budgetSpreader, error) {
	spread, err := app.store.GetBudgetSpread()
	if err != nil {
		return budgetSpreader{}, err
	}
	return newBudgetSpreader(spread), nil
}

// BudgetSaveSpread stores the payments to spread over a month, then has the
// budget redraw so the chart shows the difference at once.
func (app *Application) BudgetSaveSpread(c echo.Context) error {
	spread := models.BudgetSpread{Monthly: parseRuleList(c.FormValue("monthly"))}
	if err := app.store.SaveBudgetSpread(spread); err != nil {
		app.Toast(c, "Error", "Could not save the monthly bills.")
		return c.NoContent(http.StatusInternalServerError)
	}
	setup, err := app.buildSetupData()
	if err != nil {
		app.Toast(c, "Error", "Could not load category data.")
		return c.NoContent(http.StatusInternalServerError)
	}
	app.Toast(c, "Saved", "Monthly bills saved.")
	// Toast already uses HX-Trigger, so the chart's refresh rides on its own
	// header rather than overwriting the toast.
	c.Response().Header().Set("HX-Trigger-After-Swap", "refreshSummary")
	return c.Render(http.StatusOK, "budget.spread.saved", map[string]interface{}{
		"Spread":    spread,
		"SetupData": setup,
	})
}
