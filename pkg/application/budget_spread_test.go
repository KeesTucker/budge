package application

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetSpreaderShare(t *testing.T) {
	spreader := newBudgetSpreader(models.BudgetSpread{Monthly: []string{"mercury energy", ""}})

	paid := time.Date(2026, time.September, 1, 9, 0, 0, 0, time.Local)
	bill := models.Transaction{Description: "MERCURY ENERGY 12345", Amount: -300, Date: paid}
	shop := models.Transaction{Description: "Countdown", Amount: -90, Date: paid}

	require.True(t, spreader.monthly(bill), "matches the description, ignoring case and spacing")
	require.False(t, spreader.monthly(shop))

	start := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.Local)
	week := spreader.share(bill, 300, start, start.AddDate(0, 0, 7))
	assert.InDelta(t, 70, week, 1, "a week of a 30 day month is 7/30 of the bill")

	var month float64
	for day := start.AddDate(0, 0, -7); day.Before(start.AddDate(0, 2, 0)); day = day.AddDate(0, 0, 7) {
		month += spreader.share(bill, 300, day, day.AddDate(0, 0, 7))
	}
	assert.InDelta(t, 300, month, 0.01, "the shares add back up to the bill")

	assert.Equal(t, 90.0, spreader.share(shop, 90, start, start.AddDate(0, 0, 7)), "anything else lands whole")
	assert.Zero(t, spreader.share(shop, 90, start.AddDate(0, 0, 7), start.AddDate(0, 0, 14)))

	assert.False(t, newBudgetSpreader(models.BudgetSpread{}).monthly(bill), "nothing named, nothing spread")
}

func TestBudgetPerformanceSpreadsMonthlyBills(t *testing.T) {
	app := newTestApp(t)

	paid := time.Now().AddDate(0, 0, -21)
	require.NoError(t, app.store.CreateTransaction(models.Transaction{
		Id: "trans_power", Description: "MERCURY ENERGY", Amount: -300, Date: paid,
	}))

	weeks := func() []float64 {
		req := httptest.NewRequest(http.MethodGet, "/budget/performance?range=w8", nil)
		rec := httptest.NewRecorder()
		c := app.http.NewContext(req, rec)
		require.NoError(t, app.BudgetPerformance(c))
		match := regexp.MustCompile(`data-actuals="([^"]*)"`).FindStringSubmatch(rec.Body.String())
		require.Len(t, match, 2)
		var actuals []float64
		require.NoError(t, json.Unmarshal([]byte(html.UnescapeString(match[1])), &actuals))
		return actuals
	}
	peak := func(values []float64) float64 {
		var top float64
		for _, v := range values {
			top = max(top, v)
		}
		return top
	}

	assert.Equal(t, 300.0, peak(weeks()), "unnamed, the bill lands in one week")

	c, rec := postForm(t, app, url.Values{"monthly": {"Mercury"}})
	require.NoError(t, app.BudgetSaveSpread(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "refreshSummary", rec.Header().Get("HX-Trigger-After-Swap"))
	assert.Contains(t, rec.Body.String(), `value="Mercury"`)

	spread := weeks()
	assert.Less(t, peak(spread), 100.0, "named monthly, no week carries more than its share")
	var total float64
	for _, v := range spread {
		total += v
	}
	assert.Greater(t, total, 150.0, "the weeks since it was paid carry most of it")
	assert.LessOrEqual(t, total, 300.01)
}

func TestBudgetSetupSpreadsMonthlyBills(t *testing.T) {
	app := newTestApp(t)

	bill := models.Transaction{Id: "trans_power", Amount: -300, Date: time.Now().AddDate(0, 0, -21)}
	bill.Merchant.Name = "Mercury"
	bill.Category.Name = "Utilities"
	bill.Category.Groups.PersonalFinance.Name = "Household"
	require.NoError(t, app.store.CreateTransaction(bill))

	actual := func() float64 {
		setup, err := app.buildSetupData()
		require.NoError(t, err)
		for _, group := range setup.Groups {
			for _, row := range group.Rows {
				if row.SubCategory == "Utilities" {
					return row.ActualWeekly
				}
			}
		}
		t.Fatal("no utilities row")
		return 0
	}

	assert.InDelta(t, 75, actual(), 0.01, "unnamed, the whole bill is in the last four weeks")

	require.NoError(t, app.store.SaveBudgetSpread(models.BudgetSpread{Monthly: []string{"mercury"}}))
	assert.Less(t, actual(), 60.0, "named, only the part of its month so far counts")
}
