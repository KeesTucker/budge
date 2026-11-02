package application

import (
	"testing"

	"github.com/TheQueenIsDead/budge/pkg/database/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenced is a bank transfer to or from a person, with what the payer
// typed against it.
func referenced(txType, particulars, reference string, amount float64) models.Transaction {
	tx := spendTransaction("", "", amount, day(-1))
	tx.Type = txType
	tx.Description = "J SMITH"
	tx.Meta.Particulars = particulars
	tx.Meta.Reference = reference
	return tx
}

func TestClassifySpendReadsReferences(t *testing.T) {
	withGroups(t)

	ownTransfer := referenced("TRANSFER", "groceries", "", -200)
	ownTransfer.Category.Name = "Transfer"
	foodyPayee := referenced("PAYMENT", "", "rent", -50)
	foodyPayee.Description = "FOODY'S LTD"
	foodyPayee.Merchant.Name = "Foody's"
	inDescription := referenced("PAYMENT", "", "", 20)
	inDescription.Description = "Rowe,Amelie G Groceries"

	tests := []struct {
		name     string
		tx       models.Transaction
		expected string
		matched  bool
	}{
		{"paying a flatmate for groceries", referenced("PAYMENT", "Groceries", "", -80), "groceries", true},
		{"being paid back for petrol", referenced("DIRECT CREDIT", "petrol", "", 40), "petrol", true},
		{"a power share", referenced("PAYMENT", "", "POWER-SEPT", -95), "power", true},
		{"takeaways", referenced("PAYMENT", "", "takeaways fri", -25), "eating-out", true},
		{"a merchant named in a reference", referenced("PAYMENT", "", "hello fresh", -60), "groceries", true},
		{"however it is written", referenced("PAYMENT", "HELLO-FRESH", "wk2", -60), "groceries", true},
		{"paid back for one", referenced("DIRECT CREDIT", "", "HelloFresh", 30), "groceries", true},
		{"gas is petrol", referenced("PAYMENT", "", "gas money", -30), "petrol", true},
		{"no keyword in the reference", referenced("PAYMENT", "rent", "", -400), "", false},
		{"a keyword inside a longer word does not match", referenced("PAYMENT", "", "seafood", -30), "", false},
		{"between the owner's own accounts", ownTransfer, "", false},
		{"a merchant's name is not a reference", foodyPayee, "", false},
		{"a reference the bank put in the description", inDescription, "groceries", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := classify(tt.tx)
			assert.Equal(t, tt.matched, ok)
			assert.Equal(t, tt.expected, key)
		})
	}
}

func TestBuildSpendBucketsNetsRepayments(t *testing.T) {
	withGroups(t)
	group, ok := SpendGroupByKey("groceries")
	require.True(t, ok)
	transactions := filedAll(
		spendTransaction("Supermarkets and grocery stores", "PAK'nSAVE", -150, day(-1)),
		referenced("PAYMENT", "groceries", "", -50),
		referenced("DIRECT CREDIT", "", "groceries", 75),
	)

	buckets := BuildSpendBuckets(transactions, group, CadenceWeekly, 1, now)
	require.Len(t, buckets, 1)
	assert.InDelta(t, 125, buckets[0].Total, 0.001, "150 at the shop and 50 to a flatmate, less 75 paid back")
	assert.Equal(t, 2, buckets[0].Count, "being paid back is not a purchase")
}

func TestBuildSpendMerchantsLeavesOutRepayers(t *testing.T) {
	withGroups(t)
	group, _ := SpendGroupByKey("groceries")
	repaid := referenced("DIRECT CREDIT", "groceries", "", 75)
	repaid.Description = "A FLATMATE"
	transactions := filedAll(
		spendTransaction("Supermarkets and grocery stores", "PAK'nSAVE", -150, day(-1)),
		repaid,
	)

	merchants := BuildSpendMerchants(transactions, group, day(-7), day(1), 5)
	require.Len(t, merchants, 1)
	assert.Equal(t, "PAK'nSAVE", merchants[0].Merchant)
}

func TestSpendSummaryBarsBelowZero(t *testing.T) {
	summary := SpendSummary{Buckets: []SpendBucket{{Total: 100}, {Total: -20}}}
	bars := summary.Bars()
	assert.Equal(t, 100.0, bars[0].Pct)
	assert.Zero(t, bars[1].Pct, "a week paid back more than was spent draws empty")
}

func TestClassifySpendNamedMerchants(t *testing.T) {
	withGroups(t)
	tests := []struct {
		name string
		tx   models.Transaction
	}{
		{"named by Akahu, under another category", spendTransaction("Cafes and restaurants", "HelloFresh", -120, now)},
		{"only in the description", func() models.Transaction {
			tx := spendTransaction("", "", -120, now)
			tx.Description = "HELLO FRESH NZ AUCKLAND"
			return tx
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := classify(tt.tx)
			assert.True(t, ok)
			assert.Equal(t, "groceries", key)
		})
	}

	refund := spendTransaction("", "HelloFresh", 120, now)
	_, ok := classify(refund)
	assert.False(t, ok, "a refund is not read by merchant, only by reference")
}

// ownRules swaps a built in group's keywords and merchants for the owner's,
// the way the settings page saves them.
func ownRules(key string, keywords, merchants []string) func(*models.CategorySettings) {
	return func(s *models.CategorySettings) {
		category, _ := groupCategory(*s, key)
		s.Rules = replaceGroupRules(s.Rules, category, models.MatchKeyword, keywords)
		s.Rules = replaceGroupRules(s.Rules, category, models.MatchMerchant, merchants)
	}
}

func TestClassifySpendOwnersRules(t *testing.T) {
	withGroups(t, ownRules("groceries", []string{"veges", "Farmers Market"}, []string{"Ooooby"}))

	tests := []struct {
		name     string
		tx       models.Transaction
		expected string
		matched  bool
	}{
		{"an added keyword", referenced("PAYMENT", "", "VEGES box", -40), "groceries", true},
		{"a keyword of two words is a phrase", referenced("PAYMENT", "", "farmers-market sat", -40), "groceries", true},
		{"an added merchant", spendTransaction("Online shopping", "OOOOBY LTD", -55, now), "groceries", true},
		{"a built in keyword the owner removed", referenced("PAYMENT", "groceries", "", -80), "", false},
		{"a group the owner left alone keeps its own", referenced("PAYMENT", "petrol", "", -50), "petrol", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := classify(tt.tx)
			assert.Equal(t, tt.matched, ok)
			assert.Equal(t, tt.expected, key)
		})
	}

	views := spendRuleViews()
	assert.Equal(t, "veges, Farmers Market", views[0].Keywords, "the settings card reads the rules back")
	assert.Equal(t, "Ooooby", views[0].Merchants)
}

func TestParseRuleList(t *testing.T) {
	assert.Equal(t, []string{"groceries", "Hello Fresh", "food"},
		parseRuleList(" groceries ,Hello   Fresh,, GROCERIES\nfood "))
	assert.Empty(t, parseRuleList(" , \n "))
}

func TestClassifySpendKeepsAkahuOnly(t *testing.T) {
	tx := referenced("PAYMENT", "", "groceries", -2200)
	tx.Id = "trans_bond"
	withGroups(t, func(s *models.CategorySettings) {
		s.Overrides = append(s.Overrides, models.CategoryOverride{TransactionID: "trans_bond", AkahuOnly: true})
	})

	_, ok := classify(tx)
	assert.False(t, ok, "kept on Akahu's category, no rule puts it in a group")
	assert.True(t, BuildTransactionRow(filed(tx)).Assigned, "and it shows as the owner's choice")
}

func TestClassifySpendMerchantInDescription(t *testing.T) {
	withGroups(t)

	order := spendTransaction("Taxi, rideshare, and on-demand transport services", "Uber", -32, now)
	order.Description = "Uber *Eats H"
	key, ok := classify(order)
	assert.True(t, ok, "Akahu files Uber Eats under Uber, and only the description tells")
	assert.Equal(t, "eating-out", key)

	ride := spendTransaction("Taxi, rideshare, and on-demand transport services", "Uber", -18, now)
	ride.Description = "Uber *Trip H"
	_, ok = classify(ride)
	assert.False(t, ok, "a ride is still a ride")
}

func TestClassifySpendDualFuelRetailer(t *testing.T) {
	key, ok := ClassifySpend(spendTransaction("Electricity and gas services", "Genesis", -240, now))
	assert.True(t, ok, "a retailer selling both is still power")
	assert.Equal(t, "power", key)
}

func TestClassifySpendHonoursOverrides(t *testing.T) {
	tx := referenced("TRANSFER", "", "", 40)
	tx.Id = "trans_market"
	tx.Category.Name = "Transfer"
	withGroups(t)
	_, ok := classify(tx)
	require.False(t, ok, "no rule catches a transfer with no reference")

	withGroups(t, func(s *models.CategorySettings) {
		s.Overrides = append(s.Overrides, models.CategoryOverride{TransactionID: "trans_market", Broad: models.SpendGroupsBroad, Specific: "Groceries"})
	})
	key, ok := classify(tx)
	assert.True(t, ok)
	assert.Equal(t, "groceries", key, "a transaction filed by hand under a group's category counts there")
	assert.True(t, BuildTransactionRow(filed(tx)).Assigned)
}
