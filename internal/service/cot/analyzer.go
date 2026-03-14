package cot

import (
	"context"
	"fmt"
	"log"
	"math"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/mathutil"
)

// Analyzer computes all 20+ COT metrics from raw positioning data.
// It processes historical records to derive net positions, ratios,
// momentum, sentiment, concentration, and generates trading signals.
type Analyzer struct {
	cotRepo ports.COTRepository
	fetcher *Fetcher
}

// NewAnalyzer creates a COT analyzer.
func NewAnalyzer(repo ports.COTRepository, fetcher *Fetcher) *Analyzer {
	return &Analyzer{
		cotRepo: repo,
		fetcher: fetcher,
	}
}

// AnalyzeAll fetches latest data, computes metrics for all contracts,
// and stores the results. Returns computed analyses.
func (a *Analyzer) AnalyzeAll(ctx context.Context) ([]domain.COTAnalysis, error) {
	contracts := domain.DefaultCOTContracts // FIX: was domain.DefaultContracts() - it's a var, not func

	// Fetch latest records
	records, err := a.fetcher.FetchLatest(ctx, contracts)
	if err != nil {
		return nil, fmt.Errorf("fetch latest: %w", err)
	}

	// Save raw records
	if err := a.cotRepo.SaveRecords(ctx, records); err != nil {
		log.Printf("[cot] warn: save records: %v", err)
	}

	var analyses []domain.COTAnalysis

	for _, record := range records {
		// Get historical data for index calculation (52 weeks)
		history, err := a.cotRepo.GetHistory(ctx, record.ContractCode, 52)
		if err != nil {
			log.Printf("[cot] warn: get history for %s: %v", record.ContractCode, err)
			history = []domain.COTRecord{record} // use just current if no history
		}

		analysis := a.computeMetrics(record, history)
		analyses = append(analyses, analysis)
	}

	// Save analyses
	if err := a.cotRepo.SaveAnalyses(ctx, analyses); err != nil {
		return analyses, fmt.Errorf("save analyses: %w", err)
	}

	log.Printf("[cot] analyzed %d contracts", len(analyses))
	return analyses, nil
}

// AnalyzeContract computes metrics for a single contract.
func (a *Analyzer) AnalyzeContract(ctx context.Context, contractCode string) (*domain.COTAnalysis, error) {
	latest, err := a.cotRepo.GetLatest(ctx, contractCode)
	if err != nil {
		return nil, fmt.Errorf("get latest: %w", err)
	}

	history, err := a.cotRepo.GetHistory(ctx, contractCode, 52)
	if err != nil {
		history = []domain.COTRecord{*latest}
	}

	analysis := a.computeMetrics(*latest, history)
	return &analysis, nil
}

// computeMetrics calculates all 20+ COT metrics from a record + history.
func (a *Analyzer) computeMetrics(current domain.COTRecord, history []domain.COTRecord) domain.COTAnalysis {
	// FIX: COTAnalysis uses Contract (COTContract), not ContractCode/Currency strings
	analysis := domain.COTAnalysis{
		Contract:   findContractByCode(current.ContractCode), // FIX: was ContractCode: current.ContractCode
		ReportDate: current.ReportDate,
	}

	// === Core Position Metrics ===

	// 1. Net positions (FIX: field names match domain exactly)
	// COTRecord fields are float64, not int64
	analysis.NetPosition = current.SpecLong - current.SpecShort       // FIX: was SpecNetPosition
	analysis.NetCommercial = current.CommLong - current.CommShort     // FIX: was CommNetPosition
	analysis.NetSmallSpec = current.SmallLong - current.SmallShort    // FIX: was SmallNetPosition

	// 2. Net change (week-over-week)
	analysis.NetChange = current.SpecLongChange - current.SpecShortChange
	analysis.CommNetChange = current.CommLongChange - current.CommShortChange
	commNetChange := analysis.CommNetChange
	smallNetChange := current.SmallLongChange - current.SmallShortChange
	_ = smallNetChange

	// 3. Long/Short ratios (FIX: field names)
	analysis.LongShortRatio = safeRatio(current.SpecLong, current.SpecShort) // FIX: was SpecLongShortRatio
	analysis.CommLSRatio = safeRatio(current.CommLong, current.CommShort)     // FIX: was CommLongShortRatio

	// 4. Percentage of Open Interest (FIX: field names)
	if current.OpenInterest > 0 {
		oi := current.OpenInterest
		analysis.PctOfOI = analysis.NetPosition / oi * 100   // FIX: was SpecPctOfOI
		analysis.CommPctOfOI = analysis.NetCommercial / oi * 100
	}

	// 5. Open Interest change %
	if len(history) > 1 {
		prevOI := history[1].OpenInterest // history[0] = current
		if prevOI > 0 {
			analysis.OIPctChange = (current.OpenInterest - prevOI) / prevOI * 100
		}
	}

	// === Index Metrics (Larry Williams COT Index) ===

	if len(history) >= 3 {
		// 6. COT Index for speculators (0-100)
		specNets := extractNets(history, func(r domain.COTRecord) float64 { return r.SpecLong - r.SpecShort })
		analysis.COTIndex = computeCOTIndex(specNets)

		// 7. COT Index for commercials
		commNets := extractNets(history, func(r domain.COTRecord) float64 { return r.CommLong - r.CommShort })
		analysis.COTIndexComm = computeCOTIndex(commNets)
	}

	// === Momentum Metrics ===

	if len(history) >= 4 {
		// 8. Speculator momentum (4-week)
		specNets4 := extractNets(history[:minInt(5, len(history))], func(r domain.COTRecord) float64 {
			return r.SpecLong - r.SpecShort
		})
		analysis.SpecMomentum4W = mathutil.Momentum(specNets4, 4)

		// 9. Commercial momentum (4-week)
		commNets4 := extractNets(history[:minInt(5, len(history))], func(r domain.COTRecord) float64 {
			return r.CommLong - r.CommShort
		})
		analysis.CommMomentum4W = mathutil.Momentum(commNets4, 4)

		// 10. Momentum direction (FIX: MomentumDir is domain.MomentumDirection type)
		analysis.MomentumDir = classifyMomentumDir(analysis.SpecMomentum4W, analysis.CommMomentum4W)
	}

	// === Sentiment & Signal Metrics ===

	// 11. Overall sentiment score (-100 to +100)
	analysis.SentimentScore = computeSentiment(analysis)

	// 12-14. Individual trader signals
	analysis.CommercialSignal = classifySignal(analysis.COTIndexComm, analysis.CommMomentum4W, true)
	analysis.SpeculatorSignal = classifySignal(analysis.COTIndex, analysis.SpecMomentum4W, false)
	analysis.SmallSpecSignal = classifySmallSpec(analysis)

	// === Advanced Metrics ===

	// 15. Divergence flag (commercials vs speculators moving opposite)
	analysis.DivergenceFlag = detectDivergence(analysis.NetChange, commNetChange)

	// 16. Crowding index (how one-sided is positioning)
	analysis.CrowdingIndex = computeCrowding(current)

	// 17-18. Concentration (top 4/8 traders)
	analysis.Top4Concentration = (current.Top4Long + current.Top4Short) / 2
	analysis.Top8Concentration = (current.Top8Long + current.Top8Short) / 2

	// === Spread as % of OI ===
	if current.OpenInterest > 0 {
		analysis.SpreadPctOfOI = current.SpecSpread / current.OpenInterest * 100
	}

	// === Extreme Detection ===
	analysis.IsExtremeBull = analysis.COTIndex > 90
	analysis.IsExtremeBear = analysis.COTIndex < 10
	analysis.CommExtremeBull = analysis.COTIndexComm > 90
	analysis.CommExtremeBear = analysis.COTIndexComm < 10

	// === Smart Money vs Dumb Money ===
	analysis.SmartDumbDivergence = (analysis.NetPosition > 0 && analysis.NetCommercial < 0) ||
		(analysis.NetPosition < 0 && analysis.NetCommercial > 0)

	// === Signal Strength ===
	analysis.SignalStrength = classifySignalStrength(analysis)

	return analysis
}

// --- computation helpers ---

// findContractByCode looks up a COTContract from DefaultCOTContracts by code.
func findContractByCode(code string) domain.COTContract {
	for _, c := range domain.DefaultCOTContracts {
		if c.Code == code {
			return c
		}
	}
	// Return a minimal contract if not found
	return domain.COTContract{Code: code}
}

// computeCOTIndex implements the Larry Williams COT Index formula:
// Index = (Current Net - Min Net) / (Max Net - Min Net) * 100
// FIX: Uses float64 throughout (was int64)
func computeCOTIndex(nets []float64) float64 {
	if len(nets) < 3 {
		return 50.0 // neutral if insufficient data
	}

	current := nets[0]
	minVal, maxVal := nets[0], nets[0]

	for _, n := range nets {
		if n < minVal {
			minVal = n
		}
		if n > maxVal {
			maxVal = n
		}
	}

	span := maxVal - minVal
	if span == 0 {
		return 50.0
	}

	return mathutil.Clamp((current-minVal)/span*100, 0, 100)
}

// computeSentiment creates a composite score from all factors.
func computeSentiment(a domain.COTAnalysis) float64 {
	score := 0.0

	// COT Index contribution (40% weight)
	indexScore := (a.COTIndex - 50) * 2
	score += indexScore * 0.40

	// Commercial positioning (30% weight)
	commScore := (50 - a.COTIndexComm) * 2
	score += commScore * 0.30

	// Momentum contribution (20% weight)
	if a.SpecMomentum4W > 0 {
		score += 20
	} else if a.SpecMomentum4W < 0 {
		score -= 20
	}

	// Crowding penalty (10% weight)
	if a.CrowdingIndex > 70 {
		score -= 10
	} else if a.CrowdingIndex < 30 {
		score += 10
	}

	return mathutil.Clamp(score, -100, 100)
}

// classifySignal generates a directional signal from COT index + momentum.
func classifySignal(cotIndex, momentum float64, isCommercial bool) string {
	threshHigh := 75.0
	threshLow := 25.0
	if isCommercial {
		threshHigh, threshLow = threshLow, threshHigh
	}

	switch {
	case cotIndex >= threshHigh && momentum > 0:
		if isCommercial {
			return "STRONG_BEARISH"
		}
		return "STRONG_BULLISH"
	case cotIndex >= threshHigh:
		if isCommercial {
			return "BEARISH"
		}
		return "BULLISH"
	case cotIndex <= threshLow && momentum < 0:
		if isCommercial {
			return "STRONG_BULLISH"
		}
		return "STRONG_BEARISH"
	case cotIndex <= threshLow:
		if isCommercial {
			return "BULLISH"
		}
		return "BEARISH"
	default:
		return "NEUTRAL"
	}
}

// classifySmallSpec generates small speculator signal (contrarian indicator).
func classifySmallSpec(a domain.COTAnalysis) string {
	if a.NetSmallSpec > 0 && a.CrowdingIndex > 65 {
		return "CROWD_LONG"
	}
	if a.NetSmallSpec < 0 && a.CrowdingIndex > 65 {
		return "CROWD_SHORT"
	}
	return "NEUTRAL"
}

// classifyMomentumDir determines the overall positioning momentum direction.
// FIX: Returns domain.MomentumDirection type (was string)
func classifyMomentumDir(specMom, commMom float64) domain.MomentumDirection {
	if specMom > 0 && commMom < 0 {
		return domain.MomentumBuilding // spec building, comm unwinding
	}
	if specMom < 0 && commMom > 0 {
		return domain.MomentumReversing // spec reducing, comm adding
	}
	if math.Abs(specMom) < 100 && math.Abs(commMom) < 100 {
		return domain.MomentumStable
	}
	if specMom < 0 {
		return domain.MomentumUnwinding
	}
	return domain.MomentumBuilding
}

// classifySignalStrength rates the conviction level of the COT signal.
func classifySignalStrength(a domain.COTAnalysis) domain.SignalStrength {
	absSentiment := math.Abs(a.SentimentScore)
	switch {
	case absSentiment > 60 && (a.IsExtremeBull || a.IsExtremeBear):
		return domain.SignalStrong
	case absSentiment > 40:
		return domain.SignalModerate
	case absSentiment > 20:
		return domain.SignalWeak
	default:
		return domain.SignalNeutral
	}
}

// detectDivergence checks if commercials and speculators are moving in opposite directions.
// FIX: Takes float64 params instead of accessing non-existent fields
func detectDivergence(specNetChange, commNetChange float64) bool {
	specDir := signF(specNetChange)
	commDir := signF(commNetChange)

	if math.Abs(specNetChange) < 1000 || math.Abs(commNetChange) < 1000 {
		return false
	}

	return specDir != 0 && commDir != 0 && specDir != commDir
}

// computeCrowding measures how one-sided positioning is (0-100).
func computeCrowding(r domain.COTRecord) float64 {
	totalLong := r.SpecLong + r.CommLong + r.SmallLong
	totalShort := r.SpecShort + r.CommShort + r.SmallShort
	total := totalLong + totalShort

	if total == 0 {
		return 50.0
	}

	longPct := totalLong / total * 100
	deviation := math.Abs(longPct - 50)

	return mathutil.Clamp(deviation*2, 0, 100)
}

// --- utility helpers ---

// FIX: extractNets returns []float64 (was []int64)
func extractNets(history []domain.COTRecord, fn func(domain.COTRecord) float64) []float64 {
	nets := make([]float64, len(history))
	for i, r := range history {
		nets[i] = fn(r)
	}
	return nets
}

// safeRatio computes a/b safely, returning 0 or 999.99 on edge cases.
// FIX: Parameters are float64 (were cast from int64)
func safeRatio(a, b float64) float64 {
	if b == 0 {
		if a > 0 {
			return 999.99
		}
		return 0
	}
	return math.Round(a/b*100) / 100
}

// signF returns the sign of a float64 value.
// FIX: Renamed from sign() and takes float64 (was int64)
func signF(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

// minInt returns the minimum of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
