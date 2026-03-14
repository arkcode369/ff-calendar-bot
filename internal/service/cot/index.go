package cot

import (
	"fmt"
	"math"
	"strings"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/pkg/fmtutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/mathutil"
)

// IndexCalculator computes extended COT Index variants beyond
// the basic Larry Williams method in analyzer.go.
// This includes multi-timeframe indices, rate-of-change indices,
// and composite scoring for cross-contract comparison.
type IndexCalculator struct{}

// NewIndexCalculator creates an index calculator.
func NewIndexCalculator() *IndexCalculator {
	return &IndexCalculator{}
}

// MultiTimeframeIndex computes COT indices across different lookback periods.
// Returns indices for 13-week, 26-week, and 52-week windows.
type MultiTimeframeIndex struct {
	ContractCode string
	Currency     string
	Index13W     float64 // Quarter (seasonal)
	Index26W     float64 // Half-year (medium-term)
	Index52W     float64 // Full year (long-term)
	Average      float64 // Weighted average: 50% 26W + 30% 52W + 20% 13W
	Trend        string  // RISING, FALLING, FLAT based on 13W vs 52W
}

// ComputeMultiTimeframe calculates COT indices across 3 lookback windows.
func (ic *IndexCalculator) ComputeMultiTimeframe(history []domain.COTRecord) *MultiTimeframeIndex {
	if len(history) < 13 {
		return nil
	}

	specNets := extractNetsFloat(history, func(r domain.COTRecord) float64 {
		return r.SpecLong - r.SpecShort
	})

	mtf := &MultiTimeframeIndex{
		ContractCode: history[0].ContractCode,
	}

	// 13-week index
	window13 := specNets[:min(13, len(specNets))]
	mtf.Index13W = computeCOTIndexFromFloats(window13)

	// 26-week index
	if len(specNets) >= 26 {
		window26 := specNets[:26]
		mtf.Index26W = computeCOTIndexFromFloats(window26)
	} else {
		mtf.Index26W = mtf.Index13W
	}

	// 52-week index
	if len(specNets) >= 52 {
		window52 := specNets[:52]
		mtf.Index52W = computeCOTIndexFromFloats(window52)
	} else {
		mtf.Index52W = mtf.Index26W
	}

	// Weighted average: emphasize medium-term
	mtf.Average = mtf.Index26W*0.50 + mtf.Index52W*0.30 + mtf.Index13W*0.20

	// Trend: compare short vs long timeframe
	diff := mtf.Index13W - mtf.Index52W
	switch {
	case diff > 15:
		mtf.Trend = "RISING"
	case diff < -15:
		mtf.Trend = "FALLING"
	default:
		mtf.Trend = "FLAT"
	}

	return mtf
}

// IndexRateOfChange measures how fast the COT Index is changing.
// Useful for detecting inflection points before the index hits extremes.
type IndexRateOfChange struct {
	ContractCode string
	ROC1W        float64 // 1-week change
	ROC4W        float64 // 4-week change
	Acceleration float64 // ROC of ROC (second derivative)
	Signal       string  // ACCELERATING_BULL, DECELERATING_BULL, etc.
}

// ComputeROC calculates rate-of-change metrics for the COT Index.
func (ic *IndexCalculator) ComputeROC(history []domain.COTRecord) *IndexRateOfChange {
	if len(history) < 8 {
		return nil
	}

	// Compute weekly COT indices to get ROC
	specNets := extractNetsFloat(history, func(r domain.COTRecord) float64 {
		return r.SpecLong - r.SpecShort
	})

	// Calculate rolling indices
	var weeklyIndices []float64
	for i := 0; i < min(8, len(specNets)); i++ {
		window := specNets[i:min(i+26, len(specNets))]
		if len(window) >= 3 {
			weeklyIndices = append(weeklyIndices, computeCOTIndexFromFloats(window))
		}
	}

	if len(weeklyIndices) < 5 {
		return nil
	}

	roc := &IndexRateOfChange{
		ContractCode: history[0].ContractCode,
	}

	// 1-week ROC
	roc.ROC1W = weeklyIndices[0] - weeklyIndices[1]

	// 4-week ROC
	if len(weeklyIndices) >= 5 {
		roc.ROC4W = weeklyIndices[0] - weeklyIndices[4]
	}

	// Acceleration: current ROC vs previous ROC
	prevROC1W := weeklyIndices[1] - weeklyIndices[2]
	roc.Acceleration = roc.ROC1W - prevROC1W

	// Classify signal
	roc.Signal = classifyROCSignal(roc.ROC4W, roc.Acceleration)

	return roc
}

// CompositeScore creates a normalized 0-100 score for cross-contract comparison.
// Combines: COT Index (40%), Momentum (25%), Concentration (15%), Crowding (20%).
type CompositeScore struct {
	ContractCode string
	Currency     string
	Score        float64
	Components   map[string]float64
	Rank         int // Set externally after comparing all contracts
}

// ComputeComposite calculates a composite positioning score.
func (ic *IndexCalculator) ComputeComposite(analysis domain.COTAnalysis) CompositeScore {
	cs := CompositeScore{
		ContractCode: analysis.Contract.Code,
		Currency:     analysis.Contract.Currency,
		Components:   make(map[string]float64),
	}

	// COT Index component (0-100, 40% weight)
	indexComponent := analysis.COTIndex
	cs.Components["cot_index"] = indexComponent

	// Momentum component (normalize to 0-100, 25% weight)
	// Positive momentum -> higher score
	momentumNorm := 50.0
	if analysis.SpecMomentum4W != 0 {
		// Cap momentum at +/-50000 for normalization
		capped := mathutil.Clamp(analysis.SpecMomentum4W, -50000, 50000)
		momentumNorm = 50 + (capped/50000)*50
	}
	cs.Components["momentum"] = momentumNorm

	// Concentration component (inverted: high concentration = risk, 15% weight)
	concNorm := 100 - analysis.Top4Concentration
	cs.Components["concentration"] = concNorm

	// Anti-crowding component (inverted: high crowd = contrarian, 20% weight)
	antiCrowd := 100 - analysis.CrowdingIndex
	cs.Components["anti_crowd"] = antiCrowd

	// Weighted composite
	cs.Score = indexComponent*0.40 + momentumNorm*0.25 + concNorm*0.15 + antiCrowd*0.20
	cs.Score = mathutil.Clamp(cs.Score, 0, 100)

	return cs
}

// FormatMultiTimeframe creates a display string for MTF index.
func FormatMultiTimeframe(mtf *MultiTimeframeIndex) string {
	if mtf == nil {
		return "Insufficient data"
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s COT Index (Multi-TF):\n", mtf.Currency))
	b.WriteString(fmt.Sprintf("  13W: %s %s\n", fmtutil.FmtNum(mtf.Index13W, 1), fmtutil.COTIndexBar(mtf.Index13W, 10)))
	b.WriteString(fmt.Sprintf("  26W: %s %s\n", fmtutil.FmtNum(mtf.Index26W, 1), fmtutil.COTIndexBar(mtf.Index26W, 10)))
	b.WriteString(fmt.Sprintf("  52W: %s %s\n", fmtutil.FmtNum(mtf.Index52W, 1), fmtutil.COTIndexBar(mtf.Index52W, 10)))
	b.WriteString(fmt.Sprintf("  Avg: %s | Trend: %s", fmtutil.FmtNum(mtf.Average, 1), mtf.Trend))

	return b.String()
}

// --- helpers ---

func computeCOTIndexFromFloats(values []float64) float64 {
	if len(values) < 3 {
		return 50.0
	}

	current := values[0]
	minVal, maxVal := values[0], values[0]

	for _, v := range values {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}

	span := maxVal - minVal
	if span == 0 {
		return 50.0
	}

	return mathutil.Clamp((current-minVal)/span*100, 0, 100)
}

func classifyROCSignal(roc4w, acceleration float64) string {
	isPositive := roc4w > 0
	isAccelerating := acceleration > 0

	switch {
	case isPositive && isAccelerating:
		return "ACCELERATING_BULL"
	case isPositive && !isAccelerating:
		return "DECELERATING_BULL"
	case !isPositive && !isAccelerating:
		return "ACCELERATING_BEAR"
	case !isPositive && isAccelerating:
		return "DECELERATING_BEAR"
	default:
		return "NEUTRAL"
	}
}

func absFloat(v float64) float64 {
	return math.Abs(v)
}

func extractNetsFloat(history []domain.COTRecord, fn func(domain.COTRecord) float64) []float64 {
	out := make([]float64, len(history))
	for i, r := range history {
		out[i] = fn(r)
	}
	return out
}
