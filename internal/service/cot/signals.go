package cot

import (
	"fmt"
	"math"
	"strings"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/pkg/fmtutil"
	"github.com/arkcode369/ff-calendar-bot/pkg/mathutil"
)

// SignalDetector identifies actionable trading signals from COT data.
// It combines multiple positioning metrics to generate high-confidence
// signals: smart money moves, extreme positioning, and divergences.
type SignalDetector struct{}

// NewSignalDetector creates a signal detector.
func NewSignalDetector() *SignalDetector {
	return &SignalDetector{}
}

// Signal represents an actionable COT-based trading signal.
type Signal struct {
	ContractCode string
	Currency     string
	Type         SignalType
	Direction    string  // BULLISH, BEARISH
	Strength     int     // 1-5 (5 = strongest)
	Confidence   float64 // 0-100%
	Description  string
	Factors      []string // contributing factors
}

// SignalType categorizes the kind of signal.
type SignalType string

const (
	SignalSmartMoney       SignalType = "SMART_MONEY"
	SignalExtreme          SignalType = "EXTREME_POSITIONING"
	SignalDivergence       SignalType = "DIVERGENCE"
	SignalMomentumShift    SignalType = "MOMENTUM_SHIFT"
	SignalConcentration    SignalType = "CONCENTRATION"
	SignalCrowdContrarian  SignalType = "CROWD_CONTRARIAN"
)

// DetectAll runs all signal detectors on a set of analyses.
func (sd *SignalDetector) DetectAll(analyses []domain.COTAnalysis, historyMap map[string][]domain.COTRecord) []Signal {
	var signals []Signal

	for _, a := range analyses {
		history := historyMap[a.Contract.Code]

		// Run each detector
		if s := sd.detectSmartMoney(a, history); s != nil {
			signals = append(signals, *s)
		}
		if s := sd.detectExtreme(a); s != nil {
			signals = append(signals, *s)
		}
		if s := sd.detectDivergence(a, history); s != nil {
			signals = append(signals, *s)
		}
		if s := sd.detectMomentumShift(a, history); s != nil {
			signals = append(signals, *s)
		}
		if s := sd.detectConcentrationRisk(a); s != nil {
			signals = append(signals, *s)
		}
		if s := sd.detectCrowdContrarian(a); s != nil {
			signals = append(signals, *s)
		}
	}

	// Sort by strength descending
	sortSignals(signals)
	return signals
}

// detectSmartMoney identifies when commercial hedgers are making significant moves.
// Commercials are considered "smart money" — they have physical exposure and
// tend to be right at extremes.
func (sd *SignalDetector) detectSmartMoney(a domain.COTAnalysis, history []domain.COTRecord) *Signal {
	// Need significant commercial position change
	commChangeAbs := math.Abs(a.CommNetChange)
	if commChangeAbs < 5000 {
		return nil
	}

	// Check if commercial index is at extreme
	if a.COTIndexComm < 20 || a.COTIndexComm > 80 {
		// Smart money at extreme = high conviction signal
		direction := "BULLISH"
		if a.COTIndexComm > 80 {
			// Commercials extremely long = they're hedging heavy short exposure
			// = underlying asset likely to fall (contrarian)
			direction = "BEARISH"
		}

		strength := 3
		if commChangeAbs > 15000 {
			strength = 5
		} else if commChangeAbs > 10000 {
			strength = 4
		}

		confidence := mathutil.Clamp(commChangeAbs/200, 30, 95)

		factors := []string{
			fmt.Sprintf("Commercial net change: %s", fmtutil.FmtNumSigned(a.CommNetChange, 0)),
			fmt.Sprintf("Commercial COT Index: %.1f", a.COTIndexComm),
			fmt.Sprintf("Commercial signal: %s", a.CommercialSignal),
		}

		return &Signal{
			ContractCode: a.Contract.Code,
			Currency:     a.Contract.Currency,
			Type:         SignalSmartMoney,
			Direction:    direction,
			Strength:     strength,
			Confidence:   confidence,
			Description:  fmt.Sprintf("Smart money %s: Commercials at extreme (%.0f) with large position change", direction, a.COTIndexComm),
			Factors:      factors,
		}
	}

	return nil
}

// detectExtreme identifies when positioning reaches historically extreme levels.
func (sd *SignalDetector) detectExtreme(a domain.COTAnalysis) *Signal {
	// Check COT Index extremes
	if a.COTIndex > 10 && a.COTIndex < 90 {
		return nil
	}

	// Additional confirmation from Z-Score
	hasZConfirm := math.Abs(a.WillcoIndex-50) > 30

	direction := "BULLISH"
	if a.COTIndex >= 90 {
		// Speculators extremely bullish = potential contrarian sell
		direction = "BEARISH"
	}

	strength := 3
	if (a.COTIndex <= 5 || a.COTIndex >= 95) && hasZConfirm {
		strength = 5
	} else if a.COTIndex <= 5 || a.COTIndex >= 95 {
		strength = 4
	}

	confidence := 60.0
	if hasZConfirm {
		confidence = 80.0
	}

	factors := []string{
		fmt.Sprintf("Speculator COT Index: %.1f (extreme)", a.COTIndex),
		fmt.Sprintf("Willco Index: %.2f", a.WillcoIndex),
		fmt.Sprintf("Percentile: %.0f%%", a.COTIndex),
	}

	return &Signal{
		ContractCode: a.Contract.Code,
		Currency:     a.Contract.Currency,
		Type:         SignalExtreme,
		Direction:    direction,
		Strength:     strength,
		Confidence:   confidence,
		Description:  fmt.Sprintf("Extreme positioning: Spec COT Index at %.0f (contrarian %s)", a.COTIndex, direction),
		Factors:      factors,
	}
}

// detectDivergence identifies when commercials and speculators are moving
// in opposite directions — a classic setup for reversals.
func (sd *SignalDetector) detectDivergence(a domain.COTAnalysis, history []domain.COTRecord) *Signal {
	if !a.DivergenceFlag {
		return nil
	}

	// Check if divergence is persistent (at least 2 consecutive weeks)
	consecutive := 1
	if len(history) >= 3 {
		for i := 1; i < min(4, len(history)-1); i++ {
			prevSpecChg := history[i].SpecLongChange - history[i].SpecShortChange
			prevCommChg := history[i].CommLongChange - history[i].CommShortChange
			if (prevSpecChg > 0 && prevCommChg < 0) || (prevSpecChg < 0 && prevCommChg > 0) {
				consecutive++
			} else {
				break
			}
		}
	}

	if consecutive < 2 {
		return nil // single-week divergence is noise
	}

	// Direction follows commercials (smart money)
	direction := "BULLISH"
	if a.CommNetChange < 0 {
		direction = "BEARISH"
	}

	strength := min(consecutive+1, 5)
	confidence := mathutil.Clamp(float64(consecutive)*25, 40, 90)

	factors := []string{
		fmt.Sprintf("Divergence persisting %d weeks", consecutive),
		fmt.Sprintf("Spec net change: %s", fmtutil.FmtNumSigned(a.NetChange, 0)),
		fmt.Sprintf("Comm net change: %s", fmtutil.FmtNumSigned(a.CommNetChange, 0)),
		fmt.Sprintf("Momentum direction: %s", a.MomentumDir),
	}

	return &Signal{
		ContractCode: a.Contract.Code,
		Currency:     a.Contract.Currency,
		Type:         SignalDivergence,
		Direction:    direction,
		Strength:     strength,
		Confidence:   confidence,
		Description:  fmt.Sprintf("Spec/Commercial divergence (%d weeks): Commercials %s while specs go opposite", consecutive, strings.ToLower(direction)),
		Factors:      factors,
	}
}

// detectMomentumShift identifies when positioning momentum changes direction.
func (sd *SignalDetector) detectMomentumShift(a domain.COTAnalysis, history []domain.COTRecord) *Signal {
	if len(history) < 4 {
		return nil
	}

	// Compute current vs previous 4-week momentum
	currentMom := a.SpecMomentum4W

	// Calculate previous week's momentum for comparison
	if len(history) < 5 {
		return nil
	}
	prevNets := extractNetsFloat(history[1:min(6, len(history))], func(r domain.COTRecord) float64 {
		return float64(r.SpecLong - r.SpecShort)
	})
	prevMom := mathutil.Momentum(prevNets, 4)

	// Detect sign change (momentum flip)
	if (currentMom > 0 && prevMom > 0) || (currentMom < 0 && prevMom < 0) || currentMom == 0 {
		return nil
	}

	direction := "BULLISH"
	if currentMom < 0 {
		direction = "BEARISH"
	}

	magnitude := math.Abs(currentMom - prevMom)
	strength := 3
	if magnitude > 20000 {
		strength = 5
	} else if magnitude > 10000 {
		strength = 4
	}

	confidence := mathutil.Clamp(magnitude/300, 40, 85)

	factors := []string{
		fmt.Sprintf("Momentum flipped from %s to %s", fmtutil.FmtNumSigned(prevMom, 0), fmtutil.FmtNumSigned(currentMom, 0)),
		fmt.Sprintf("Magnitude of shift: %s", fmtutil.FmtNum(magnitude, 0)),
		fmt.Sprintf("Spec net position: %s", fmtutil.FmtNumSigned(a.NetPosition, 0)),
	}

	return &Signal{
		ContractCode: a.Contract.Code,
		Currency:     a.Contract.Currency,
		Type:         SignalMomentumShift,
		Direction:    direction,
		Strength:     strength,
		Confidence:   confidence,
		Description:  fmt.Sprintf("Momentum shift to %s: 4W spec momentum flipped sign", direction),
		Factors:      factors,
	}
}

// detectConcentrationRisk flags when top traders hold unusually large positions.
func (sd *SignalDetector) detectConcentrationRisk(a domain.COTAnalysis) *Signal {
	// Top4 concentration > 50% is concerning
	if a.Top4Concentration < 50 {
		return nil
	}

	// High concentration = potential for sharp reversal
	direction := "BEARISH" // default: concentrated long = risk of unwind
	if a.NetPosition < 0 {
		direction = "BULLISH" // concentrated short = risk of short squeeze
	}

	strength := 3
	if a.Top4Concentration > 65 {
		strength = 5
	} else if a.Top4Concentration > 55 {
		strength = 4
	}

	confidence := mathutil.Clamp(a.Top4Concentration-20, 30, 80)

	factors := []string{
		fmt.Sprintf("Top 4 traders: %.1f%% of OI", a.Top4Concentration),
		fmt.Sprintf("Top 8 traders: %.1f%% of OI", a.Top8Concentration),
		fmt.Sprintf("Spec net: %s", fmtutil.FmtNumSigned(a.NetPosition, 0)),
	}

	return &Signal{
		ContractCode: a.Contract.Code,
		Currency:     a.Contract.Currency,
		Type:         SignalConcentration,
		Direction:    direction,
		Strength:     strength,
		Confidence:   confidence,
		Description:  fmt.Sprintf("Concentration risk: Top 4 hold %.0f%% of OI — vulnerable to %s", a.Top4Concentration, map[string]string{"BEARISH": "unwind", "BULLISH": "squeeze"}[direction]),
		Factors:      factors,
	}
}

// detectCrowdContrarian flags when small speculators are extremely crowded.
func (sd *SignalDetector) detectCrowdContrarian(a domain.COTAnalysis) *Signal {
	if a.CrowdingIndex < 70 {
		return nil
	}

	// Crowd is wrong at extremes — contrarian signal
	direction := "BEARISH"
	if a.NetSmallSpec < 0 {
		direction = "BULLISH" // crowd is short = contrarian buy
	}

	strength := 3
	if a.CrowdingIndex > 85 {
		strength = 5
	} else if a.CrowdingIndex > 75 {
		strength = 4
	}

	confidence := mathutil.Clamp(a.CrowdingIndex-30, 40, 85)

	factors := []string{
		fmt.Sprintf("Crowding index: %.1f (extreme)", a.CrowdingIndex),
		fmt.Sprintf("Small spec net: %s", fmtutil.FmtNumSigned(a.NetSmallSpec, 0)),
		fmt.Sprintf("Small spec signal: %s", a.SmallSpecSignal),
	}

	return &Signal{
		ContractCode: a.Contract.Code,
		Currency:     a.Contract.Currency,
		Type:         SignalCrowdContrarian,
		Direction:    direction,
		Strength:     strength,
		Confidence:   confidence,
		Description:  fmt.Sprintf("Crowd contrarian %s: Small specs crowded (%.0f) — fading the crowd", direction, a.CrowdingIndex),
		Factors:      factors,
	}
}

// FormatSignals creates a Telegram-formatted signal summary.
func FormatSignals(signals []Signal) string {
	if len(signals) == 0 {
		return "No actionable COT signals detected."
	}

	var b strings.Builder
	b.WriteString("=== COT SIGNALS ===")

	for i, s := range signals {
		if i >= 10 {
			b.WriteString(fmt.Sprintf("\n... and %d more signals", len(signals)-10))
			break
		}

		strengthBar := strings.Repeat("|", s.Strength)
		dirIcon := "^" // bullish
		if s.Direction == "BEARISH" {
			dirIcon = "v" // bearish
		}

		b.WriteString(fmt.Sprintf("\n\n%s %s %s [%s]\n",
			dirIcon, s.Currency, s.Type, strengthBar))
		b.WriteString(fmt.Sprintf("%s\n", s.Description))
		b.WriteString(fmt.Sprintf("Confidence: %.0f%%\n", s.Confidence))

		for _, f := range s.Factors {
			b.WriteString(fmt.Sprintf("  - %s\n", f))
		}
	}

	return b.String()
}

// sortSignals sorts by strength descending, then confidence descending.
func sortSignals(signals []Signal) {
	for i := 1; i < len(signals); i++ {
		for j := i; j > 0; j-- {
			if signals[j].Strength > signals[j-1].Strength ||
				(signals[j].Strength == signals[j-1].Strength && signals[j].Confidence > signals[j-1].Confidence) {
				signals[j], signals[j-1] = signals[j-1], signals[j]
			} else {
				break
			}
		}
	}
}
