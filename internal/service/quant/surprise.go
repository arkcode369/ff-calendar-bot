package quant

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/fmtutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/mathutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// SurpriseCalculator computes the Economic Surprise Index for each currency.
// It measures how actual economic data compares to consensus forecasts,
// weighted by event impact and recency (exponential decay).
//
// Formula per event:
//   surprise = (actual - forecast) / stddev(historical_surprises)
//   weighted = surprise * impactWeight * decayFactor
//
// Rolling Surprise Index = sum(weighted surprises) over N days
type SurpriseCalculator struct {
	eventRepo    ports.EventRepository
	surpriseRepo ports.SurpriseRepository
	windowDays   int     // rolling window (default 30)
	decayFactor  float64 // exponential decay lambda (default 0.05)
}

// NewSurpriseCalculator creates a surprise calculator with default parameters.
func NewSurpriseCalculator(eventRepo ports.EventRepository, surpriseRepo ports.SurpriseRepository) *SurpriseCalculator {
	return &SurpriseCalculator{
		eventRepo:    eventRepo,
		surpriseRepo: surpriseRepo,
		windowDays:   30,
		decayFactor:  0.05,
	}
}

// ComputeAll calculates surprise indices for all major currencies.
func (sc *SurpriseCalculator) ComputeAll(ctx context.Context) (map[string]*domain.SurpriseIndex, error) {
	currencies := []string{"USD", "EUR", "GBP", "JPY", "AUD", "NZD", "CAD", "CHF"}
	results := make(map[string]*domain.SurpriseIndex)

	for _, ccy := range currencies {
		idx, err := sc.ComputeForCurrency(ctx, ccy)
		if err != nil {
			log.Printf("[surprise] warn: %s: %v", ccy, err)
			continue
		}
		results[ccy] = idx
	}

	log.Printf("[surprise] computed indices for %d currencies", len(results))
	return results, nil
}

// ComputeForCurrency calculates the rolling surprise index for a single currency.
func (sc *SurpriseCalculator) ComputeForCurrency(ctx context.Context, currency string) (*domain.SurpriseIndex, error) {
	now := timeutil.NowWIB()
	start := now.AddDate(0, 0, -sc.windowDays)

	// Get events with actual values in the window
	events, err := sc.eventRepo.GetEventsByDateRange(ctx, start, now)
	if err != nil {
		return nil, fmt.Errorf("get events: %w", err)
	}

	// Filter to this currency's events with actual + forecast
	var relevant []domain.FFEvent
	for _, ev := range events {
		if ev.Currency == currency && ev.Actual != "" && ev.Forecast != "" {
			relevant = append(relevant, ev)
		}
	}

	if len(relevant) == 0 {
		return &domain.SurpriseIndex{
			Currency:      currency,
			RollingScore:  0,
			WindowDays:    sc.windowDays,
			DecayHalfLife: math.Log(2) / sc.decayFactor,
			Timestamp:     now,
		}, nil
	}

	// Calculate individual surprise scores
	var components []domain.SurpriseScore
	var rollingSum float64

	for _, ev := range relevant {
		score := sc.computeEventSurprise(ev, now)
		if score == nil {
			continue
		}

		components = append(components, *score)
		rollingSum += score.WeightedImpact

		// Save individual score
		if err := sc.surpriseRepo.SaveSurprise(ctx, *score); err != nil {
			log.Printf("[surprise] warn: save score: %v", err)
		}
	}

	comps := make([]domain.SurpriseComponent, 0, len(components))
	for _, c := range components {
		comps = append(comps, domain.SurpriseComponent{
			EventName:      c.EventName,
			Timestamp:      c.Timestamp,
			WeightedImpact: c.WeightedImpact,
		})
	}

	idx := &domain.SurpriseIndex{
		Currency:      currency,
		RollingScore:  rollingSum,
		Components:    comps,
		WindowDays:    sc.windowDays,
		DecayHalfLife: math.Log(2) / sc.decayFactor,
		Timestamp:     now,
	}

	return idx, nil
}

// ComputeRevisionMomentum tracks the direction and streak of data revisions.
func (sc *SurpriseCalculator) ComputeRevisionMomentum(ctx context.Context, currency string, days int) (*domain.RevisionMomentum, error) {
	revisions, err := sc.eventRepo.GetRevisions(ctx, currency, days)
	if err != nil {
		return nil, fmt.Errorf("get revisions: %w", err)
	}

	if len(revisions) == 0 {
		return &domain.RevisionMomentum{
			Currency:  currency,
			Direction: "NEUTRAL",
			Streak:    0,
			Score:     0,
		}, nil
	}

	// Count upward vs downward revisions
	upward := 0
	downward := 0
	for _, rev := range revisions {
		switch rev.Direction {
		case "upward":
			upward++
		case "downward":
			downward++
		}
	}

	// Calculate streak (consecutive same-direction revisions)
	streak := 0
	if len(revisions) > 0 {
		firstDir := revisions[0].Direction
		for _, rev := range revisions {
			if rev.Direction == firstDir {
				streak++
			} else {
				break
			}
		}
	}

	direction := "NEUTRAL"
	if upward > downward*2 {
		direction = "IMPROVING"
	} else if downward > upward*2 {
		direction = "DETERIORATING"
	} else if upward > downward {
		direction = "SLIGHTLY_IMPROVING"
	} else if downward > upward {
		direction = "SLIGHTLY_DETERIORATING"
	}

	// Score: -100 to +100
	total := upward + downward
	score := 0.0
	if total > 0 {
		score = float64(upward-downward) / float64(total) * 100
	}

	return &domain.RevisionMomentum{
		Currency:  currency,
		Direction: direction,
		Streak:    streak,
		Score:     score,
	}, nil
}

// computeEventSurprise calculates the surprise score for a single event.
func (sc *SurpriseCalculator) computeEventSurprise(ev domain.FFEvent, now time.Time) *domain.SurpriseScore {
	actual := parseNumericValue(ev.Actual)
	forecast := parseNumericValue(ev.Forecast)

	// Need both values to compute surprise
	if math.IsNaN(actual) || math.IsNaN(forecast) {
		return nil
	}

	// Raw surprise
	rawSurprise := actual - forecast

	// Normalized surprise: divide by forecast magnitude to make cross-event comparable
	normalized := 0.0
	if math.Abs(forecast) > 0.001 {
		normalized = rawSurprise / math.Abs(forecast) * 100
	} else {
		normalized = rawSurprise * 100 // small base: use raw
	}

	// Impact weight: High=3, Medium=2, Low=1
	impactWeight := float64(ev.Impact)
	if impactWeight == 0 {
		impactWeight = 1
	}

	// Time decay: more recent events have higher weight
	daysAgo := now.Sub(ev.Date).Hours() / 24
	decay := mathutil.ExponentialDecay(1.0, daysAgo, math.Log(2)/sc.decayFactor)

	// Weighted impact
	weighted := normalized * impactWeight * decay

	return &domain.SurpriseScore{
		EventID:           ev.ID,
		EventName:         ev.Title,
		Currency:          ev.Currency,
		Surprise:          rawSurprise,
		NormalizedSurprise: normalized,
		WeightedImpact:    weighted,
		Timestamp:         ev.Date,
	}
}

// FormatSurpriseIndex creates a display string for the surprise index.
func FormatSurpriseIndex(indices map[string]*domain.SurpriseIndex) string {
	if len(indices) == 0 {
		return "No surprise data available."
	}

	var b strings.Builder
	b.WriteString("=== ECONOMIC SURPRISE INDEX ===\n")

	// Sort currencies by score
	type ccyScore struct {
		Currency string
		Score    float64
	}
	var sorted []ccyScore
	for ccy, idx := range indices {
		sorted = append(sorted, ccyScore{ccy, idx.RollingScore})
	}
	// Sort descending
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Score > sorted[j-1].Score; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	for _, cs := range sorted {
		idx := indices[cs.Currency]
		bar := surpriseBar(idx.RollingScore)
		b.WriteString(fmt.Sprintf("%s: %s %s (%d events)\n",
			cs.Currency,
			fmtutil.FmtNumSigned(idx.RollingScore, 1),
			bar,
			len(idx.Components)))
	}

	return b.String()
}

// surpriseBar creates a visual bar for surprise score.
func surpriseBar(score float64) string {
	if score > 10 {
		return "[++++]"
	} else if score > 5 {
		return "[+++ ]"
	} else if score > 0 {
		return "[++  ]"
	} else if score > -5 {
		return "[--  ]"
	} else if score > -10 {
		return "[--- ]"
	}
	return "[----]"
}

// parseNumericValue parses a string like "1.5%", "200K", "-0.3" into float64.
func parseNumericValue(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return math.NaN()
	}

	// Remove common suffixes
	multiplier := 1.0
	s = strings.ReplaceAll(s, ",", "")

	if strings.HasSuffix(s, "%") {
		s = strings.TrimSuffix(s, "%")
	} else if strings.HasSuffix(s, "K") || strings.HasSuffix(s, "k") {
		s = s[:len(s)-1]
		multiplier = 1000
	} else if strings.HasSuffix(s, "M") || strings.HasSuffix(s, "m") {
		s = s[:len(s)-1]
		multiplier = 1000000
	} else if strings.HasSuffix(s, "B") || strings.HasSuffix(s, "b") {
		s = s[:len(s)-1]
		multiplier = 1000000000
	} else if strings.HasSuffix(s, "T") || strings.HasSuffix(s, "t") {
		s = s[:len(s)-1]
		multiplier = 1000000000000
	}

	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return math.NaN()
	}

	return f * multiplier
}
