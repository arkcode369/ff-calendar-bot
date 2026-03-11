package quant

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/fmtutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/mathutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// VolatilityPredictor estimates expected pip movements for upcoming news events
// based on historical actual-vs-forecast deviations and their market impact.
//
// Method:
//   1. Fetch historical data for the event (12-24 months)
//   2. Compute historical surprise magnitudes
//   3. Calculate average pip move per unit of surprise
//   4. Apply confidence intervals and recent volatility weighting
type VolatilityPredictor struct {
	eventRepo ports.EventRepository
}

// NewVolatilityPredictor creates a volatility predictor.
func NewVolatilityPredictor(eventRepo ports.EventRepository) *VolatilityPredictor {
	return &VolatilityPredictor{
		eventRepo: eventRepo,
	}
}

// PredictForEvent estimates the expected pip movement for a specific event.
func (vp *VolatilityPredictor) PredictForEvent(ctx context.Context, event domain.FFEvent) (*domain.VolatilityPrediction, error) {
	// Get historical data
	history, err := vp.eventRepo.GetEventHistory(ctx, event.Title, event.Currency, 24)
	if err != nil || len(history) < 3 {
		// Fallback: use impact-based estimate
		return vp.fallbackPrediction(event), nil
	}

	// Compute historical surprises
	var surprises []float64
	for _, h := range history {
		actual := parseNumericValue(h.Actual)
		forecast := parseNumericValue(h.Forecast)
		if math.IsNaN(actual) || math.IsNaN(forecast) {
			continue
		}
		surprises = append(surprises, math.Abs(actual-forecast))
	}

	if len(surprises) < 3 {
		return vp.fallbackPrediction(event), nil
	}

	// Statistical analysis of surprises
	mean := mathutil.Mean(surprises)
	stdDev := mathutil.StdDev(surprises)

	// Estimate pip move using event-specific multipliers
	multiplier := vp.getEventPipMultiplier(event.Title, event.Currency)

	// Expected move = mean surprise * multiplier
	expectedPips := mean * multiplier

	// Historical average move (use recent 6 for recency bias)
	recentCount := min(6, len(surprises))
	recentMean := mathutil.Mean(surprises[:recentCount])
	histAvgMove := recentMean * multiplier

	// Confidence: more history = more confidence, lower stddev = more confidence
	cv := 0.0
	if mean > 0 {
		cv = stdDev / mean // coefficient of variation
	}
	confidence := mathutil.Clamp(100-cv*30-float64(24-len(history))*2, 20, 95)

	// Impact score (0-100): combines expected move magnitude with confidence
	impactScore := mathutil.Clamp(expectedPips*2+confidence*0.3, 0, 100)

	pred := &domain.VolatilityPrediction{
		EventName:       event.Title,
		Currency:        event.Currency,
		ExpectedPipMove: math.Round(expectedPips*10) / 10,
		HistoricalAvgMove: math.Round(histAvgMove*10) / 10,
		ImpactScore:     math.Round(impactScore),
		Confidence:      math.Round(confidence),
		SampleSize:      len(surprises),
		StdDev:          math.Round(stdDev*multiplier*10) / 10,
	}

	return pred, nil
}

// PredictUpcoming generates volatility predictions for all upcoming high-impact events.
func (vp *VolatilityPredictor) PredictUpcoming(ctx context.Context, hours int) (*domain.VolatilityForecast, error) {
	now := timeutil.NowWIB()
	end := now.Add(timeutil.Hours(hours))

	events, err := vp.eventRepo.GetEventsByDateRange(ctx, now, end)
	if err != nil {
		return nil, fmt.Errorf("get events: %w", err)
	}

	forecast := &domain.VolatilityForecast{
		Timeframe: fmt.Sprintf("Next %d hours", hours),
		UpdatedAt: now,
	}

	for _, ev := range events {
		if ev.Impact != domain.ImpactHigh && ev.Impact != domain.ImpactMedium {
			continue
		}

		pred, err := vp.PredictForEvent(ctx, ev)
		if err != nil {
			log.Printf("[volatility] warn: predict %s: %v", ev.Title, err)
			continue
		}
		pred.EventTime = ev.DateTime
		forecast.Predictions = append(forecast.Predictions, *pred)
	}

	// Compute aggregate volatility expectation
	if len(forecast.Predictions) > 0 {
		var totalImpact float64
		var maxMove float64
		for _, p := range forecast.Predictions {
			totalImpact += p.ImpactScore
			if p.ExpectedPipMove > maxMove {
				maxMove = p.ExpectedPipMove
			}
		}
		forecast.AggregateImpact = totalImpact / float64(len(forecast.Predictions))
		forecast.MaxExpectedMove = maxMove

		// Classify overall volatility
		switch {
		case forecast.AggregateImpact >= 70 || maxMove >= 80:
			forecast.VolatilityLevel = "EXTREME"
		case forecast.AggregateImpact >= 50 || maxMove >= 50:
			forecast.VolatilityLevel = "HIGH"
		case forecast.AggregateImpact >= 30 || maxMove >= 25:
			forecast.VolatilityLevel = "MODERATE"
		default:
			forecast.VolatilityLevel = "LOW"
		}
	}

	return forecast, nil
}

// fallbackPrediction creates an estimate based on event impact level only.
func (vp *VolatilityPredictor) fallbackPrediction(event domain.FFEvent) *domain.VolatilityPrediction {
	// Default pip estimates by impact level
	var expectedPips, impactScore float64

	switch event.Impact {
	case domain.ImpactHigh:
		expectedPips = 40
		impactScore = 60
	case domain.ImpactMedium:
		expectedPips = 20
		impactScore = 35
	default:
		expectedPips = 10
		impactScore = 15
	}

	// Adjust for known high-volatility events
	titleLower := strings.ToLower(event.Title)
	if strings.Contains(titleLower, "nonfarm") || strings.Contains(titleLower, "non-farm") {
		expectedPips = 80
		impactScore = 90
	} else if strings.Contains(titleLower, "cpi") || strings.Contains(titleLower, "inflation") {
		expectedPips = 60
		impactScore = 80
	} else if strings.Contains(titleLower, "gdp") {
		expectedPips = 50
		impactScore = 70
	} else if strings.Contains(titleLower, "rate decision") || strings.Contains(titleLower, "interest rate") {
		expectedPips = 70
		impactScore = 85
	} else if strings.Contains(titleLower, "employment") || strings.Contains(titleLower, "jobs") {
		expectedPips = 50
		impactScore = 65
	}

	return &domain.VolatilityPrediction{
		EventName:         event.Title,
		Currency:          event.Currency,
		ExpectedPipMove:   expectedPips,
		HistoricalAvgMove: expectedPips, // no history, use same
		ImpactScore:       impactScore,
		Confidence:        25, // low confidence without history
		SampleSize:        0,
	}
}

// getEventPipMultiplier returns a pip-per-unit-surprise multiplier.
// These are empirically derived from historical forex reactions.
func (vp *VolatilityPredictor) getEventPipMultiplier(eventName, currency string) float64 {
	titleLower := strings.ToLower(eventName)

	// NFP: ~10 pips per 10K jobs surprise
	if strings.Contains(titleLower, "nonfarm") || strings.Contains(titleLower, "non-farm") {
		return 0.001 // 1 pip per 1000 jobs
	}

	// CPI: ~20 pips per 0.1% surprise
	if strings.Contains(titleLower, "cpi") || strings.Contains(titleLower, "inflation") {
		return 200 // pips per percentage point
	}

	// GDP: ~15 pips per 0.1% surprise
	if strings.Contains(titleLower, "gdp") {
		return 150
	}

	// Rate decisions: ~30 pips per 25bps surprise
	if strings.Contains(titleLower, "rate decision") || strings.Contains(titleLower, "interest rate") {
		return 120
	}

	// Employment/unemployment: ~15 pips per 0.1% surprise
	if strings.Contains(titleLower, "employment") || strings.Contains(titleLower, "unemployment") {
		return 150
	}

	// PMI: ~8 pips per 1 point surprise
	if strings.Contains(titleLower, "pmi") || strings.Contains(titleLower, "purchasing") {
		return 8
	}

	// Trade balance: varies by currency
	if strings.Contains(titleLower, "trade balance") {
		return 5
	}

	// Retail sales: ~12 pips per 0.1% surprise
	if strings.Contains(titleLower, "retail sales") {
		return 120
	}

	// Default
	return 20
}

// FormatVolatilityForecast creates a Telegram-formatted volatility display.
func FormatVolatilityForecast(forecast *domain.VolatilityForecast) string {
	if forecast == nil || len(forecast.Predictions) == 0 {
		return "No significant volatility events upcoming."
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("=== VOLATILITY FORECAST (%s) ===\n", forecast.Timeframe))
	b.WriteString(fmt.Sprintf("Level: %s | Max Move: %s pips\n\n",
		forecast.VolatilityLevel,
		fmtutil.FmtNum(forecast.MaxExpectedMove, 0)))

	for i, p := range forecast.Predictions {
		if i >= 8 {
			b.WriteString(fmt.Sprintf("... and %d more events\n", len(forecast.Predictions)-8))
			break
		}

		timeStr := ""
		if !p.EventTime.IsZero() {
			timeStr = p.EventTime.Format("15:04")
		}

		confBar := confidenceBar(p.Confidence)
		b.WriteString(fmt.Sprintf("%s %s %s\n", timeStr, p.Currency, p.EventName))
		b.WriteString(fmt.Sprintf("  Expected: %s pips | Impact: %.0f | Conf: %s\n",
			fmtutil.FmtNum(p.ExpectedPipMove, 1), p.ImpactScore, confBar))
	}

	return b.String()
}

func confidenceBar(conf float64) string {
	switch {
	case conf >= 80:
		return "[*****]"
	case conf >= 60:
		return "[**** ]"
	case conf >= 40:
		return "[***  ]"
	case conf >= 20:
		return "[**   ]"
	default:
		return "[*    ]"
	}
}
