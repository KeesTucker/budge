package application

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TheQueenIsDead/budge/pkg/database"
	"github.com/TheQueenIsDead/budge/web"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestApp returns an Application backed by a bolt database in a temp
// directory, so handler tests never touch a real budge.bolt.db.
func newTestApp(t *testing.T) *Application {
	t.Helper()

	t.Setenv("BUDGE_BOLT_PATH", t.TempDir())
	store, err := database.NewStore()
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	categories, err := store.GetCategorySettings()
	require.NoError(t, err)
	SetCategorySettings(categories)
	store.OnCategories = SetCategorySettings
	t.Cleanup(func() { categorySettings.Store(nil) })

	tpl, err := template.New("").Funcs(templateFuncs()).ParseFS(web.Templates, "templates/*.gohtml")
	require.NoError(t, err)

	e := echo.New()
	e.Renderer = &Template{templates: tpl}

	return &Application{store: store, http: e}
}

// postForm builds an echo context for a form POST, plus the recorder holding
// whatever the handler writes.
func postForm(t *testing.T, app *Application, values url.Values) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()

	return app.http.NewContext(req, rec), rec
}

// TestBudgetSaveSalaryPreservesSavingsGoal is the regression test for the bug
// where BudgetSaveSalary rebuilt BudgetSalary from scratch, silently zeroing
// the savings goal. The income card saves on every "change" event, so this
// fired whenever a user touched any income field.
func TestBudgetSaveSalaryPreservesSavingsGoal(t *testing.T) {
	app := newTestApp(t)

	// A user sets a savings goal first.
	goalCtx, _ := postForm(t, app, url.Values{
		"savings_goal":           {"250"},
		"savings_goal_frequency": {"fortnightly"},
	})
	require.NoError(t, app.BudgetSaveSavingsGoal(goalCtx))

	saved, err := app.store.GetBudgetSalary()
	require.NoError(t, err)
	require.Equal(t, 250.0, saved.SavingsGoal)

	// They then edit their income, which posts the whole income card.
	salaryCtx, _ := postForm(t, app, url.Values{
		"salary":           {"100000"},
		"salary_frequency": {"yearly"},
		"kiwisaver_rate":   {"3"},
		"include_paye":     {"on"},
	})
	require.NoError(t, app.BudgetSaveSalary(salaryCtx))

	after, err := app.store.GetBudgetSalary()
	require.NoError(t, err)

	assert.Equal(t, 250.0, after.SavingsGoal, "savings goal must survive an income edit")
	assert.Equal(t, "fortnightly", after.SavingsGoalFrequency, "goal frequency must survive too")

	// ...and the income fields themselves are actually saved.
	assert.Equal(t, 100_000.0, after.Salary)
	assert.Equal(t, "yearly", after.SalaryFrequency)
	assert.Equal(t, 3.0, after.KiwiSaverRate)
	assert.True(t, after.IncludePAYE)
	assert.False(t, after.StudentLoan, "an unchecked box posts nothing and must clear the flag")
}

// The reverse direction: saving a goal must not clobber the income config.
func TestBudgetSaveSavingsGoalPreservesSalary(t *testing.T) {
	app := newTestApp(t)

	salaryCtx, _ := postForm(t, app, url.Values{
		"salary":           {"85000"},
		"salary_frequency": {"yearly"},
		"student_loan":     {"on"},
	})
	require.NoError(t, app.BudgetSaveSalary(salaryCtx))

	goalCtx, _ := postForm(t, app, url.Values{"savings_goal": {"100"}})
	require.NoError(t, app.BudgetSaveSavingsGoal(goalCtx))

	after, err := app.store.GetBudgetSalary()
	require.NoError(t, err)

	assert.Equal(t, 85_000.0, after.Salary, "salary must survive a savings goal edit")
	assert.True(t, after.StudentLoan)
	assert.Equal(t, 100.0, after.SavingsGoal)
	assert.Equal(t, "weekly", after.SavingsGoalFrequency, "empty frequency defaults to weekly")
}

func TestGetBudgetSalaryDefaults(t *testing.T) {
	app := newTestApp(t)

	salary, err := app.store.GetBudgetSalary()
	require.NoError(t, err)

	assert.Equal(t, "yearly", salary.SalaryFrequency)
	assert.True(t, salary.IncludePAYE)
	assert.Equal(t, 3.5, salary.KiwiSaverRate)
	assert.Zero(t, salary.Salary)
}

func TestTransactionCategoryFilesIntoGroups(t *testing.T) {
	app := newTestApp(t)

	tx := referenced("PAYMENT", "", "groceries", -2200)
	tx.Id = "trans_bond"
	require.NoError(t, app.store.CreateTransaction(tx))

	group := func() (string, bool) {
		t.Helper()
		read, err := app.store.GetTransaction(tx.Id)
		require.NoError(t, err)
		return ClassifySpend(read)
	}
	post := func(values url.Values) {
		t.Helper()
		c, rec := postForm(t, app, values)
		c.SetParamNames("id")
		c.SetParamValues(tx.Id)
		require.NoError(t, app.TransactionSetCategory(c))
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	key, ok := group()
	require.True(t, ok, "the built in keyword rule files it")
	assert.Equal(t, "groceries", key)

	post(url.Values{"akahu": {"1"}})
	_, ok = group()
	assert.False(t, ok, "kept on Akahu's category, no rule can claim it")

	post(url.Values{"category": {"Petrol"}})
	key, ok = group()
	assert.True(t, ok, "filed by hand under a group's category, it counts there")
	assert.Equal(t, "petrol", key)

	post(url.Values{"clear": {"1"}})
	key, _ = group()
	assert.Equal(t, "groceries", key, "undoing hands it back to the rules")
}
