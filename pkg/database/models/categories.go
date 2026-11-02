package models

import (
	"strings"
	"unicode"

	"github.com/TheQueenIsDead/budge/pkg/database/buckets"
)

// CustomCategory is a category of the owner's own, alongside the ones Akahu
// assigns, for rules to file transactions under.
type CustomCategory struct {
	ID       string
	Broad    string
	Specific string
	// SpendGroup is the key of the spend group the category feeds on the
	// insights page, if any.
	SpendGroup string `json:",omitempty"`
}

// SpendGroupsBroad heads the categories that feed spend groups.
const SpendGroupsBroad = "Your groups"

// How a rule's Contains is matched. The zero value is a plain text match, so
// rules saved before match kinds existed keep working.
const (
	// MatchText finds Contains in the description or merchant name.
	MatchText = ""
	// MatchMerchant finds a business however it is spaced or punctuated: in
	// a purchase's merchant name or description, or in a bank reference.
	MatchMerchant = "merchant"
	// MatchKeyword finds a word in a bank reference, such as "groceries" on a
	// payment to a flatmate. A word matches when it starts with Contains, so
	// "grocer" catches "groceries" and "food" does not catch "seafood".
	MatchKeyword = "keyword"
	// MatchType files every debit of Type, whatever it says, for cash: each
	// ATM describes itself differently.
	MatchType = "type"
)

// CategoryRule files matching transactions under a category, overriding the
// one Akahu gave them.
type CategoryRule struct {
	ID string
	// Match is how Contains is matched; see MatchText and the rest.
	Match string `json:",omitempty"`
	// Contains is what the rule looks for. A plain text rule matches it
	// case-insensitively against the description and the merchant name.
	Contains string
	// Type narrows the rule to one transaction type, such as LOAN. Empty
	// matches any type.
	Type     string
	Broad    string
	Specific string
	// IncludeTransfers lets the rule match money moved between the owner's
	// own accounts, which rules otherwise leave alone. It exists for
	// payments that are real outgoings even though both ends are the
	// owner's, such as a mortgage repayment into a loan account.
	IncludeTransfers bool
}

// TypeCountedTransfer marks the outgoing leg of a transfer that a rule has
// chosen to count as spending. Every view leaves out "TRANSFER", so giving
// the leg its own type is what brings it into spend, everywhere at once.
const TypeCountedTransfer = "COUNTED TRANSFER"

// IsTransfer reports whether a transaction moves money between the owner's
// own accounts, whether or not a rule counts it.
func (t Transaction) IsTransfer() bool {
	return t.Type == "TRANSFER" || t.Type == TypeCountedTransfer
}

// Matches reports whether the rule applies to a transaction. Transfers are
// money moving between the owner's own accounts, so a rule only matches one
// when it opts in, and then only the leg where money leaves: the leg where it
// arrives stays a transfer, so the payment is counted once and never as
// income.
func (r CategoryRule) Matches(tx Transaction) bool {
	if tx.IsTransfer() && (!r.IncludeTransfers || tx.Amount >= 0) {
		return false
	}
	if r.Match == MatchType {
		return r.Type != "" && strings.EqualFold(r.Type, tx.Type) && tx.Amount < 0
	}
	if r.Contains == "" {
		return false
	}
	if r.Type != "" && !strings.EqualFold(r.Type, tx.Type) {
		return false
	}
	switch r.Match {
	case MatchMerchant:
		return r.matchesMerchant(tx)
	case MatchKeyword:
		return keywordMatches(r.Contains, referenceText(tx))
	}
	needle := strings.ToLower(r.Contains)
	return strings.Contains(strings.ToLower(tx.Description), needle) ||
		strings.Contains(strings.ToLower(tx.Merchant.Name), needle)
}

// matchesMerchant looks for the business in a purchase's merchant name and
// description, since Akahu files several businesses under one merchant (an
// Uber Eats order is merchant "Uber"), and in any bank reference: paying a
// flatmate "Hello Fresh" is the same box as a card payment to HelloFresh.
func (r CategoryRule) matchesMerchant(tx Transaction) bool {
	m := LettersOnly(r.Contains)
	if m == "" {
		return false
	}
	if tx.Amount < 0 && (strings.Contains(LettersOnly(tx.Merchant.Name), m) || strings.Contains(LettersOnly(tx.Description), m)) {
		return true
	}
	return strings.Contains(LettersOnly(referenceText(tx)), m)
}

// referenceText is what the payer typed against a bank payment. Some banks
// give Akahu no reference fields and put the reference on the end of the
// description instead ("Rowe,Amelie G Groceries"), so a payment with no
// merchant, which is a payment to or from a person, has its description read
// too. A purchase's description is the shop's name, no clue to what was bought.
func referenceText(tx Transaction) string {
	fields := []string{tx.Meta.Particulars, tx.Meta.Code, tx.Meta.Reference}
	if tx.Merchant.Name == "" {
		fields = append(fields, tx.Description)
	}
	return strings.Join(fields, " ")
}

// keywordMatches reports whether text holds a keyword. A single word matches
// the start of any word in it. A keyword with spaces or punctuation, which an
// owner typing their own is likely to write, matches the phrase anywhere,
// ignoring spacing and punctuation.
func keywordMatches(keyword, text string) bool {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return false
	}
	if strings.ContainsFunc(keyword, func(r rune) bool { return !unicode.IsLetter(r) }) {
		k := LettersOnly(keyword)
		return k != "" && strings.Contains(LettersOnly(text), k)
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
	for _, word := range words {
		if strings.HasPrefix(word, keyword) {
			return true
		}
	}
	return false
}

// LettersOnly lower cases a name and drops everything but letters and
// digits, so "HELLO FRESH NZ" and "HelloFresh" read the same.
func LettersOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// RuleCategoryPrefix starts the category id of a transaction a rule filed.
const RuleCategoryPrefix = "rule:"

// OverrideCategoryPrefix marks the category id of a transaction the owner
// filed by hand.
const OverrideCategoryPrefix = "override:"

// KeptCategoryPrefix marks the category id of a transaction the owner kept
// on Akahu's category, beyond every rule and out of every spend group.
const KeptCategoryPrefix = "kept:"

// CategoryOverride files one transaction under a category, for the one-off
// payment no rule would describe, such as a plumber fixing the hot water
// cylinder. It wins over rules and over Akahu.
type CategoryOverride struct {
	TransactionID string
	Broad         string
	Specific      string
	// AkahuOnly keeps Akahu's own category and puts the transaction beyond
	// every rule and out of every spend group, for the odd one a rule gets
	// wrong: a large payment that happened to carry the usual "groceries"
	// reference.
	AkahuOnly bool `json:",omitempty"`
}

// CategorySettings holds the custom categories, the rules in the order they
// are tried, and the per-transaction overrides.
type CategorySettings struct {
	Categories []CustomCategory
	Rules      []CategoryRule
	Overrides  []CategoryOverride
}

// Override returns the override for a transaction, if it has one.
func (cs CategorySettings) Override(transactionID string) (CategoryOverride, bool) {
	for _, o := range cs.Overrides {
		if o.TransactionID == transactionID {
			return o, true
		}
	}
	return CategoryOverride{}, false
}

func (cs CategorySettings) Key() []byte    { return []byte("categories") }
func (cs CategorySettings) Bucket() []byte { return buckets.SettingsBucket }

// MatchingRule returns the index of the first rule that matches, or -1.
func (cs CategorySettings) MatchingRule(tx Transaction) int {
	for i, rule := range cs.Rules {
		if rule.Matches(tx) {
			return i
		}
	}
	return -1
}

// Apply recategorises transactions in place. An override for the
// transaction wins; otherwise the first matching rule does. Only the copy
// being read is changed: Akahu's own category stays in the database, so
// removing an override or a rule restores it straight away.
func (cs CategorySettings) Apply(txs []Transaction) {
	if len(cs.Rules) == 0 && len(cs.Overrides) == 0 {
		return
	}
	overrides := make(map[string]CategoryOverride, len(cs.Overrides))
	for _, o := range cs.Overrides {
		overrides[o.TransactionID] = o
	}
	for i := range txs {
		if o, ok := overrides[txs[i].Id]; ok {
			if o.AkahuOnly {
				txs[i].Category.Id = KeptCategoryPrefix + o.TransactionID
			} else {
				file(&txs[i], OverrideCategoryPrefix+o.TransactionID, o.Broad, o.Specific)
			}
			continue
		}
		if n := cs.MatchingRule(txs[i]); n >= 0 {
			rule := cs.Rules[n]
			file(&txs[i], RuleCategoryPrefix+rule.ID, rule.Broad, rule.Specific)
		}
	}
}

// file puts a transaction under a category. The ids mark where the category
// came from, and the group id is what Transaction.Categories checks before
// showing the group. Money leaving by transfer that is filed somewhere is
// counted as spending; the leg where it arrives stays a transfer.
func file(tx *Transaction, id, broad, specific string) {
	tx.Category.Id = id
	tx.Category.Name = specific
	tx.Category.Groups.PersonalFinance.Id = id
	tx.Category.Groups.PersonalFinance.Name = broad
	if tx.IsTransfer() && tx.Amount < 0 {
		tx.Type = TypeCountedTransfer
	}
}
