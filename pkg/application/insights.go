package application

import (
	"net/http"
	"slices"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/labstack/echo/v4"
)

// InsightsData drives the per-category deep dive. The page shows one group at a
// time so a phone screen carries a full chart rather than five cramped ones.
type InsightsData struct {
	Groups   []SpendGroup
	Selected SpendGroup
	Cadence  Cadence

	Summary   SpendSummary
	Merchants []models.MerchantTotal

	// Periods lists the transactions behind this period and the last, so a
	// number on the page can be traced to what made it.
	Periods []InsightPeriod

	WindowStart time.Time
	WindowEnd   time.Time
}

// insightsHistory is how far back the page reads. Twelve monthly buckets need
// a year, plus a month of slack so the oldest bucket is not clipped.
const insightsHistory = 14

func (app *Application) Insights(c echo.Context) error {

	now := time.Now()

	transactions, err := app.store.ReadTransactionsByDate(now.AddDate(0, -insightsHistory, 0), now)
	if err != nil {
		app.Toast(c, "Error", "Could not load spending insights.")
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	groups := SpendGroups()

	// An unknown or missing group falls back to the first tracked one rather
	// than erroring, so a stale bookmark still lands somewhere useful.
	selected := groups[0]
	if group, ok := SpendGroupByKey(c.QueryParam("group")); ok {
		selected = group
	}

	cadence := Cadence(c.QueryParam("cadence"))
	if !cadence.Valid() {
		cadence = selected.Cadence
	}

	summary := BuildSpendSummary(transactions, selected, cadence, now)
	start, end := summary.Window()

	return c.Render(http.StatusOK, "insights", InsightsData{
		Groups:      groups,
		Selected:    selected,
		Cadence:     cadence,
		Summary:     summary,
		Merchants:   BuildSpendMerchants(transactions, selected, start, end, 12),
		Periods:     BuildInsightPeriods(transactions, summary, now.Location()),
		WindowStart: start,
		WindowEnd:   end,
	})
}

// InsightPeriod is one period's transactions in the selected group.
type InsightPeriod struct {
	Label string
	Start time.Time
	Total float64
	Rows  []TransactionRow

	// CarriedIn and CarriedOut are paybacks moved between periods, so the
	// total can be squared with the rows listed under it.
	CarriedIn  float64
	CarriedOut float64
}

// insightPeriods is how many periods the breakdown lists: the one in progress
// and the last whole one, which are the two the headline figures compare.
const insightPeriods = 2

// BuildInsightPeriods lists the transactions the summary counted in its most
// recent periods, newest period and newest transaction first.
func BuildInsightPeriods(transactions []models.Transaction, summary SpendSummary, location *time.Location) []InsightPeriod {
	buckets := summary.Buckets
	if len(buckets) > insightPeriods {
		buckets = buckets[len(buckets)-insightPeriods:]
	}

	periods := make([]InsightPeriod, 0, len(buckets))
	for i := len(buckets) - 1; i >= 0; i-- {
		bucket := buckets[i]
		label := "Last " + summary.Cadence.Label()
		if bucket.Partial {
			label = "This " + summary.Cadence.Label()
		}
		period := InsightPeriod{
			Label:      label,
			Start:      bucket.Start,
			Total:      bucket.Total,
			CarriedIn:  bucket.CarriedIn,
			CarriedOut: bucket.CarriedOut,
		}
		for _, tx := range transactions {
			key, ok := ClassifySpend(tx)
			if !ok || key != summary.Group.Key {
				continue
			}
			date := tx.Date.In(location)
			if date.Before(bucket.Start) || !date.Before(bucket.End) {
				continue
			}
			period.Rows = append(period.Rows, BuildTransactionRow(tx))
		}
		slices.SortFunc(period.Rows, func(a, b TransactionRow) int {
			return b.Date.Compare(a.Date)
		})
		periods = append(periods, period)
	}
	return periods
}
