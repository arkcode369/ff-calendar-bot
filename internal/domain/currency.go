package domain

import (
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Currency Score — Per-Currency Fundamental Composite
// ---------------------------------------------------------------------------

// CurrencyCode represents a major currency.
type CurrencyCode string

const (
	CurrUSD CurrencyCode = "USD"
	CurrEUR CurrencyCode = "EUR"
	CurrGBP CurrencyCode = "GBP"
	CurrJPY CurrencyCode = "JPY"
	CurrCHF CurrencyCode = "CHF"
	CurrAUD CurrencyCode = "AUD"
	CurrCAD CurrencyCode = "CAD"
	CurrNZD CurrencyCode = "NZD"
)

// MajorCurrencies lists all 8 major currencies in standard order.
var MajorCurrencies = []CurrencyCode{
	CurrUSD, CurrEUR, CurrGBP, CurrJPY,
	CurrCHF, CurrAUD, CurrCAD, CurrNZD,
}

// CurrencyScore holds the composite fundamental score for a single currency.
// Each sub-score is 0-100, combined into a weighted composite.
type CurrencyScore struct {
	Code      CurrencyCode `json:"code"`      // e.g., "USD"
	Timestamp time.Time    `json:"timestamp"` // When this was last calculated

	// Sub-scores (each 0-100)
	InterestRateScore  float64 `json:"interest_rate_score"`  // Central bank rate trajectory (25%)
	InflationScore     float64 `json:"inflation_score"`      // CPI/PPI trend vs target (20%)
	GDPScore           float64 `json:"gdp_score"`            // GDP growth momentum (20%)
	EmploymentScore    float64 `json:"employment_score"`     // Jobs data composite (20%)
	COTScore           float64 `json:"cot_score"`            // Speculator positioning via COT (15%)

	// Composite
	CompositeScore float64 `json:"composite_score"` // Weighted average (0-100)

	// Raw data references (latest values used for scoring)
	LatestRate       float64 `json:"latest_rate"`        // Current interest rate
	LatestCPI        float64 `json:"latest_cpi"`         // Latest CPI YoY
	LatestGDP        float64 `json:"latest_gdp"`         // Latest GDP QoQ
	LatestUnemployment float64 `json:"latest_unemployment"` // Latest unemployment rate

	// Trend indicators
	RateTrend       string `json:"rate_trend"`        // "HIKING", "CUTTING", "HOLD"
	InflationTrend  string `json:"inflation_trend"`   // "RISING", "FALLING", "STABLE"
	GrowthTrend     string `json:"growth_trend"`      // "EXPANDING", "CONTRACTING", "STABLE"
	EmploymentTrend string `json:"employment_trend"` // "IMPROVING", "DETERIORATING", "STABLE"
}

// ComputeComposite calculates the weighted composite score.
func (cs *CurrencyScore) ComputeComposite() {
	cs.CompositeScore = cs.InterestRateScore*0.25 +
		cs.InflationScore*0.20 +
		cs.GDPScore*0.20 +
		cs.EmploymentScore*0.20 +
		cs.COTScore*0.15
}

// StrengthLabel returns a text label for the currency strength.
func (cs *CurrencyScore) StrengthLabel() string {
	switch {
	case cs.CompositeScore >= 75:
		return "VERY STRONG"
	case cs.CompositeScore >= 60:
		return "STRONG"
	case cs.CompositeScore >= 45:
		return "NEUTRAL"
	case cs.CompositeScore >= 30:
		return "WEAK"
	default:
		return "VERY WEAK"
	}
}

// ---------------------------------------------------------------------------
// Currency Ranking — Sorted Rankings of All 8 Majors
// ---------------------------------------------------------------------------

// CurrencyRanking holds the ranked list of all major currencies.
type CurrencyRanking struct {
	Timestamp time.Time       `json:"timestamp"`
	Rankings  []RankedCurrency `json:"rankings"` // Sorted strongest to weakest
}

// RankedCurrency is a currency with its rank position.
type RankedCurrency struct {
	Rank           int          `json:"rank"`            // 1 = strongest, 8 = weakest
	Score          CurrencyScore `json:"score"`           // Full score details
	PreviousRank   int          `json:"previous_rank"`   // Last week's rank
	RankChange     int          `json:"rank_change"`     // Positive = improved, negative = declined
}

// SortByComposite sorts the rankings from strongest to weakest.
func (cr *CurrencyRanking) SortByComposite() {
	sort.Slice(cr.Rankings, func(i, j int) bool {
		return cr.Rankings[i].Score.CompositeScore > cr.Rankings[j].Score.CompositeScore
	})
	for i := range cr.Rankings {
		cr.Rankings[i].Rank = i + 1
	}
}

// GetByCode returns the RankedCurrency for a given currency code.
func (cr *CurrencyRanking) GetByCode(code CurrencyCode) *RankedCurrency {
	for i := range cr.Rankings {
		if cr.Rankings[i].Score.Code == code {
			return &cr.Rankings[i]
		}
	}
	return nil
}

// Strongest returns the #1 ranked currency.
func (cr *CurrencyRanking) Strongest() *RankedCurrency {
	if len(cr.Rankings) == 0 {
		return nil
	}
	return &cr.Rankings[0]
}

// Weakest returns the last ranked currency.
func (cr *CurrencyRanking) Weakest() *RankedCurrency {
	if len(cr.Rankings) == 0 {
		return nil
	}
	return &cr.Rankings[len(cr.Rankings)-1]
}

// ---------------------------------------------------------------------------
// Pair Analysis — Differential Between Two Currencies
// ---------------------------------------------------------------------------

// PairAnalysis compares two currencies for directional trading bias.
type PairAnalysis struct {
	Pair          string       `json:"pair"`           // e.g., "EURUSD"
	Base          CurrencyCode `json:"base"`           // e.g., "EUR"
	Quote         CurrencyCode `json:"quote"`          // e.g., "USD"
	Timestamp     time.Time    `json:"timestamp"`

	// Differentials (Base - Quote)
	ScoreDifferential float64 `json:"score_differential"` // Base composite - Quote composite
	RateDifferential  float64 `json:"rate_differential"`  // Base rate - Quote rate

	// Direction & Strength
	Direction string  `json:"direction"` // "LONG" (favor base), "SHORT" (favor quote), "NEUTRAL"
	Strength  float64 `json:"strength"`  // Absolute differential (0-100)

	// Sub-differentials
	RateDiff       float64 `json:"rate_diff"`       // Interest rate score diff
	InflationDiff  float64 `json:"inflation_diff"`  // Inflation score diff
	GDPDiff        float64 `json:"gdp_diff"`        // GDP score diff
	EmploymentDiff float64 `json:"employment_diff"` // Employment score diff
	COTDiff        float64 `json:"cot_diff"`        // COT score diff
}

// IsActionable returns true if the differential is large enough to trade.
func (pa *PairAnalysis) IsActionable() bool {
	return pa.Strength >= 15 // Minimum 15-point differential
}
