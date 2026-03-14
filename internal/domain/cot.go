package domain

import "time"

// ---------------------------------------------------------------------------
// COT Contract Definitions
// ---------------------------------------------------------------------------

// COTContract defines a tracked CFTC futures contract.
type COTContract struct {
	Code     string `json:"code"`     // CFTC contract market code
	Name     string `json:"name"`     // Human-readable name (e.g., "Euro FX")
	Symbol   string `json:"symbol"`   // Trading symbol (e.g., "EURUSD")
	Currency string `json:"currency"` // Related currency code (e.g., "EUR")
	Inverse  bool   `json:"inverse"`  // True if contract is inverse to currency (e.g., USD Index)
}

// DefaultCOTContracts returns the 7 core tracked contracts plus expandable extras.
var DefaultCOTContracts = []COTContract{
	{Code: "099741", Name: "Euro FX", Symbol: "6E", Currency: "EUR", Inverse: false},
	{Code: "096742", Name: "British Pound", Symbol: "6B", Currency: "GBP", Inverse: false},
	{Code: "097741", Name: "Japanese Yen", Symbol: "6J", Currency: "JPY", Inverse: false},
	{Code: "092741", Name: "Swiss Franc", Symbol: "6S", Currency: "CHF", Inverse: false},
	{Code: "232741", Name: "Australian Dollar", Symbol: "6A", Currency: "AUD", Inverse: false},
	{Code: "090741", Name: "Canadian Dollar", Symbol: "6C", Currency: "CAD", Inverse: false},
	{Code: "112741", Name: "NZ Dollar", Symbol: "6N", Currency: "NZD", Inverse: false},
	{Code: "098662", Name: "US Dollar Index", Symbol: "DX", Currency: "USD", Inverse: true},
	{Code: "088691", Name: "Gold", Symbol: "GC", Currency: "XAU", Inverse: false},
	{Code: "067651", Name: "Crude Oil WTI", Symbol: "CL", Currency: "OIL", Inverse: false},
	{Code: "043602", Name: "10-Year T-Note", Symbol: "ZN", Currency: "BOND", Inverse: false},
}

// ---------------------------------------------------------------------------
// COT Record — Raw CFTC Data
// ---------------------------------------------------------------------------

// COTRecord represents raw COT data from a single CFTC weekly report.
type COTRecord struct {
	// Identification
	ContractCode string    `json:"contract_code"` // CFTC code
	ContractName string    `json:"contract_name"` // Market name
	ReportDate   time.Time `json:"report_date"`   // Report as-of date (Tuesday)

	// Open Interest
	OpenInterest    float64 `json:"open_interest"`
	OpenInterestOld float64 `json:"open_interest_old"` // Previous week for change calc

	// Commercial (Hedger) positions
	CommLong  float64 `json:"comm_long"`
	CommShort float64 `json:"comm_short"`

	// Non-Commercial (Large Speculator) positions
	SpecLong  float64 `json:"spec_long"`
	SpecShort float64 `json:"spec_short"`
	SpecSpread float64 `json:"spec_spread"` // Spread positions (calendar spreads)

	// Non-Reportable (Small Speculator) positions
	SmallLong  float64 `json:"small_long"`
	SmallShort float64 `json:"small_short"`

	// Concentration data (Top traders)
	Top4Long  float64 `json:"top4_long"`  // % of OI held by top 4 longs
	Top4Short float64 `json:"top4_short"` // % of OI held by top 4 shorts
	Top8Long  float64 `json:"top8_long"`  // % of OI held by top 8 longs
	Top8Short float64 `json:"top8_short"` // % of OI held by top 8 shorts

	// Changes from previous week (pre-calculated if available)
	CommLongChange  float64 `json:"comm_long_change"`
	CommShortChange float64 `json:"comm_short_change"`
	SpecLongChange  float64 `json:"spec_long_change"`
	SpecShortChange float64 `json:"spec_short_change"`
	SmallLongChange  float64 `json:"small_long_change"`
	SmallShortChange float64 `json:"small_short_change"`
}

// NetCommercial returns Commercial net position (Long - Short).
func (r *COTRecord) NetCommercial() float64 {
	return r.CommLong - r.CommShort
}

// NetSpeculator returns Large Speculator net position.
func (r *COTRecord) NetSpeculator() float64 {
	return r.SpecLong - r.SpecShort
}

// NetSmallSpec returns Small Speculator net position.
func (r *COTRecord) NetSmallSpec() float64 {
	return r.SmallLong - r.SmallShort
}

// CurrencyToContract maps a currency code to the CFTC contract code used in COT data.
func CurrencyToContract(currency string) string {
	for _, c := range DefaultCOTContracts {
		if c.Currency == currency {
			return c.Code
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Momentum Direction
// ---------------------------------------------------------------------------

// MomentumDirection indicates the direction and intensity of positioning changes.
type MomentumDirection string

const (
	MomentumBuilding  MomentumDirection = "BUILDING"   // Accelerating in same direction
	MomentumUnwinding MomentumDirection = "UNWINDING"  // Reducing positions
	MomentumStable    MomentumDirection = "STABLE"     // Little change
	MomentumReversing MomentumDirection = "REVERSING"  // Changing direction
)

// ---------------------------------------------------------------------------
// Signal Strength
// ---------------------------------------------------------------------------

// SignalStrength rates the conviction level of a COT signal.
type SignalStrength string

const (
	SignalStrong   SignalStrength = "STRONG"
	SignalModerate SignalStrength = "MODERATE"
	SignalWeak     SignalStrength = "WEAK"
	SignalNeutral  SignalStrength = "NEUTRAL"
)

// ---------------------------------------------------------------------------
// COT Analysis — Computed Metrics
// ---------------------------------------------------------------------------

// COTAnalysis contains all computed metrics for a single contract.
type COTAnalysis struct {
	// Reference
	Contract   COTContract `json:"contract"`
	ReportDate time.Time   `json:"report_date"`

	// --- A. Core Positioning ---
	NetPosition     float64 `json:"net_position"`      // Speculator net (primary signal)
	NetChange       float64 `json:"net_change"`        // Week-over-week change in spec net
	CommNetChange   float64 `json:"comm_net_change"`   // Week-over-week change in commercial net
	NetCommercial   float64 `json:"net_commercial"`    // Commercial net position
	NetSmallSpec    float64 `json:"net_small_spec"`    // Small speculator net
	LongShortRatio  float64 `json:"long_short_ratio"`  // Spec Long / Spec Short
	CommLSRatio     float64 `json:"comm_ls_ratio"`     // Comm Long / Comm Short
	PctOfOI         float64 `json:"pct_of_oi"`         // Net as % of Open Interest
	CommPctOfOI     float64 `json:"comm_pct_of_oi"`    // Commercial net as % of OI

	// --- B. COT Index & Extremes ---
	COTIndex        float64 `json:"cot_index"`         // Williams COT Index (0-100) for specs
	COTIndexComm    float64 `json:"cot_index_comm"`    // COT Index for commercials
	IsExtremeBull   bool    `json:"is_extreme_bull"`   // COT Index > 90
	IsExtremeBear   bool    `json:"is_extreme_bear"`   // COT Index < 10
	CommExtremeBull bool    `json:"comm_extreme_bull"` // Commercial COT Index > 90
	CommExtremeBear bool    `json:"comm_extreme_bear"` // Commercial COT Index < 10
	WillcoIndex     float64 `json:"willco_index"`      // EMA-weighted COT Index variant

	// --- C. Smart Money vs Dumb Money ---
	CommercialSignal  string `json:"commercial_signal"`  // Contrarian signal from commercials
	SpeculatorSignal  string `json:"speculator_signal"`  // Trend-following signal from large specs
	SmallSpecSignal   string `json:"small_spec_signal"`  // Contrarian signal from small specs
	SmartDumbDivergence bool `json:"smart_dumb_divergence"` // Commercial vs Speculator divergence

	// --- D. Open Interest Analysis ---
	OIPctChange      float64 `json:"oi_pct_change"`      // OI % change week-over-week
	Top4Concentration float64 `json:"top4_concentration"` // Top 4 trader dominance %
	Top8Concentration float64 `json:"top8_concentration"` // Top 8 trader dominance %
	SpreadPctOfOI    float64 `json:"spread_pct_of_oi"`   // Spread positions as % of OI

	// --- E. Momentum & Trend ---
	SpecMomentum4W   float64           `json:"spec_momentum_4w"`   // 4-week rate of change of net spec
	SpecMomentum8W   float64           `json:"spec_momentum_8w"`   // 8-week rate of change
	CommMomentum4W   float64           `json:"comm_momentum_4w"`   // 4-week commercial momentum
	MomentumDir      MomentumDirection `json:"momentum_dir"`       // Overall momentum direction
	ConsecutiveWeeks int               `json:"consecutive_weeks"`  // Weeks in same direction

	// --- F. Advanced Signals ---
	DivergenceFlag  bool           `json:"divergence_flag"`  // Price vs positioning divergence
	CrowdingIndex   float64        `json:"crowding_index"`   // How one-sided (0-100, >80 = extreme)
	SentimentScore  float64        `json:"sentiment_score"`  // Weighted composite (-100 to +100)
	SignalStrength  SignalStrength `json:"signal_strength"`  // Overall signal conviction

	// AI interpretation (filled by Gemini)
	AINarrative string `json:"ai_narrative,omitempty"`
}

// ---------------------------------------------------------------------------
// Socrata API Response Mapping
// ---------------------------------------------------------------------------

// SocrataRecord maps the CFTC Socrata JSON response fields.
// Used to parse the raw API response before converting to COTRecord.
type SocrataRecord struct {
	ReportDate        string `json:"report_date_as_yyyy_mm_dd"`
	MarketName        string `json:"market_and_exchange_names"`
	ContractCode      string `json:"cftc_contract_market_code"`
	OpenInterest      string `json:"open_interest_all"`
	CommLong          string `json:"comm_positions_long_all"`
	CommShort         string `json:"comm_positions_short_all"`
	SpecLong          string `json:"noncomm_positions_long_all"`
	SpecShort         string `json:"noncomm_positions_short_all"`
	SpecSpread        string `json:"noncomm_positions_spread_all"`
	SmallLong         string `json:"nonrept_positions_long_all"`
	SmallShort        string `json:"nonrept_positions_short_all"`
	CommLongChange    string `json:"change_in_comm_long_all"`
	CommShortChange   string `json:"change_in_comm_short_all"`
	SpecLongChange    string `json:"change_in_noncomm_long_all"`
	SpecShortChange   string `json:"change_in_noncomm_short_all"`
	SmallLongChange   string `json:"change_in_nonrept_long_all"`
	SmallShortChange  string `json:"change_in_nonrept_short_all"`
	Top4Long          string `json:"pct_of_oi_4_or_less_long_all"`
	Top4Short         string `json:"pct_of_oi_4_or_less_short_all"`
	Top8Long          string `json:"pct_of_oi_8_or_less_long_all"`
	Top8Short         string `json:"pct_of_oi_8_or_less_short_all"`
}
