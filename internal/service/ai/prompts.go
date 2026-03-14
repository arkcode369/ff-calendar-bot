package ai

import (
	"fmt"
	"strings"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/pkg/fmtutil"
)

// SystemPrompt is the base system instruction for all financial analysis.
const SystemPrompt = `You are a senior forex fundamental analyst specializing in G8 currencies.
You analyze COT (Commitments of Traders) data, economic indicators, and news events.

Rules:
- Be concise and actionable. Use bullet points.
- Always state the directional bias (BULLISH/BEARISH/NEUTRAL) clearly.
- Cite specific numbers from the data provided.
- Note any conflicting signals or risks.
- Format for Telegram: use plain text, no markdown headers.
- Keep responses under 800 words.
- Use WIB (UTC+7) for all times.`

// --- Prompt Builders ---

// BuildCOTAnalysisPrompt creates a prompt for COT data interpretation.
func BuildCOTAnalysisPrompt(analyses []domain.COTAnalysis) string {
	var b strings.Builder
	b.WriteString("Analyze the following COT positioning data for G8 currency futures.\n")
	b.WriteString("Identify the strongest directional setups and any divergences.\n\n")

	for _, a := range analyses {
		b.WriteString(fmt.Sprintf("--- %s (%s) | Report: %s ---\n",
			a.Contract.Code, a.Contract.Currency, a.ReportDate.Format("2006-01-02")))
		b.WriteString(fmt.Sprintf("Spec Net: %s (chg: %s) | L/S Ratio: %s\n",
			fmtutil.FmtNumSigned(a.NetPosition, 0),
			fmtutil.FmtNumSigned(a.NetChange, 0),
			fmtutil.FmtNum(a.LongShortRatio, 2)))
		b.WriteString(fmt.Sprintf("Comm Net: %s (momentum 4W: %s) | L/S Ratio: %s\n",
			fmtutil.FmtNumSigned(a.NetCommercial, 0),
			fmtutil.FmtNumSigned(a.CommMomentum4W, 0),
			fmtutil.FmtNum(a.CommLSRatio, 2)))
		b.WriteString(fmt.Sprintf("COT Index: Spec=%.1f Comm=%.1f\n", a.COTIndex, a.COTIndexComm))
		b.WriteString(fmt.Sprintf("Momentum 4W: Spec=%s Comm=%s\n",
			fmtutil.FmtNumSigned(a.SpecMomentum4W, 0),
			fmtutil.FmtNumSigned(a.CommMomentum4W, 0)))
		b.WriteString(fmt.Sprintf("Sentiment: %.1f | Crowding: %.1f | Divergence: %v\n",
			a.SentimentScore, a.CrowdingIndex, a.DivergenceFlag))
		b.WriteString(fmt.Sprintf("Signals: Comm=%s Spec=%s SmallSpec=%s\n",
			a.CommercialSignal, a.SpeculatorSignal, a.SmallSpecSignal))
		b.WriteString(fmt.Sprintf("Concentration: Top4=%.1f%% Top8=%.1f%%\n\n",
			a.Top4Concentration, a.Top8Concentration))
	}

	b.WriteString("\nProvide:\n")
	b.WriteString("1. Overall positioning summary (which currencies are most/least favored)\n")
	b.WriteString("2. Smart money (commercial) vs speculator alignment for each\n")
	b.WriteString("3. Top 3 actionable setups with direction and conviction level\n")
	b.WriteString("4. Key risks or conflicting signals to watch\n")

	return b.String()
}

// BuildEventImpactPrompt creates a prompt for predicting event impact.
func BuildEventImpactPrompt(event domain.FFEvent, history []domain.FFEventDetail) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Analyze the upcoming %s event for %s.\n\n", event.Title, event.Currency))
	b.WriteString(fmt.Sprintf("Event: %s\n", event.Title))
	b.WriteString(fmt.Sprintf("Currency: %s\n", event.Currency))
	b.WriteString(fmt.Sprintf("Time: %s WIB\n", event.Date.Format("2006-01-02 15:04")))
	b.WriteString(fmt.Sprintf("Forecast: %s | Previous: %s\n\n", event.Forecast, event.Previous))

	if len(history) > 0 {
		b.WriteString("Historical data (last 12 releases):\n")
		for i, h := range history {
			if i >= 12 {
				break
			}
			surprise := ""
			if h.Actual != 0 && h.Forecast != 0 {
				surprise = fmt.Sprintf(" (surprise: %.4g vs %.4g)", h.Actual, h.Forecast)
			}
			b.WriteString(fmt.Sprintf("  %s: A=%.4g F=%.4g P=%.4g%s\n",
				h.Date.Format("Jan 2006"), h.Actual, h.Forecast, h.Previous, surprise))
		}
	}

	b.WriteString("\nProvide:\n")
	b.WriteString("1. What the consensus expects and why\n")
	b.WriteString("2. Upside/downside scenarios with expected pip impact\n")
	b.WriteString("3. Historical surprise pattern (does this event tend to beat/miss?)\n")
	b.WriteString("4. Key levels to watch and recommended positioning\n")

	return b.String()
}

// BuildConfluencePrompt creates a prompt for confluence score interpretation.
func BuildConfluencePrompt(score domain.ConfluenceScore) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Interpret this multi-factor confluence analysis for %s.\n\n", score.CurrencyPair))
	b.WriteString(fmt.Sprintf("Overall Score: %.1f/100 | Bias: %s | Strongest: %s\n\n",
		score.TotalScore, score.Bias, score.StrongestFactor))

	b.WriteString("Factor breakdown:\n")
	for _, f := range score.Factors {
		b.WriteString(fmt.Sprintf("  %s (weight %.0f%%): Score=%.1f Signal=%s\n",
			f.Name, f.Weight*100, f.RawScore, f.Signal))
	}

	b.WriteString("\nProvide:\n")
	b.WriteString("1. Which factors are aligned vs conflicting\n")
	b.WriteString("2. The strongest factor driving the bias\n")
	b.WriteString("3. Whether you agree with the score direction and why\n")
	b.WriteString("4. Specific trade recommendation (entry timing, risk events)\n")

	return b.String()
}

// BuildWeeklyOutlookPrompt creates a prompt for weekly market outlook.
func BuildWeeklyOutlookPrompt(data WeeklyOutlookData) string {
	var b strings.Builder
	b.WriteString("Generate a comprehensive weekly forex fundamental outlook.\n\n")

	// COT Summary
	if len(data.COTAnalyses) > 0 {
		b.WriteString("=== COT POSITIONING ===\n")
		for _, a := range data.COTAnalyses {
			b.WriteString(fmt.Sprintf("%s: SpecNet=%s COTIdx=%.0f CommSignal=%s\n",
				a.Contract.Currency,
				fmtutil.FmtNumSigned(a.NetPosition, 0),
				a.COTIndex, a.CommercialSignal))
		}
		b.WriteString("\n")
	}

	// Economic calendar
	if len(data.HighImpactEvents) > 0 {
		b.WriteString("=== KEY EVENTS THIS WEEK ===\n")
		for _, ev := range data.HighImpactEvents {
			b.WriteString(fmt.Sprintf("%s %s: %s (F:%s P:%s)\n",
				ev.Date.Format("Mon 15:04"), ev.Currency, ev.Title,
				ev.Forecast, ev.Previous))
		}
		b.WriteString("\n")
	}

	// Surprise indices
	if len(data.SurpriseIndices) > 0 {
		b.WriteString("=== ECONOMIC SURPRISE ===\n")
		for _, idx := range data.SurpriseIndices {
			if idx == nil {
				continue
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", idx.Currency, fmtutil.FmtNumSigned(idx.RollingScore, 1)))
		}
		b.WriteString("\n")
	}

	// Currency rankings
	if data.Rankings != nil && len(data.Rankings.Rankings) > 0 {
		b.WriteString("=== CURRENCY STRENGTH ===\n")
		for _, r := range data.Rankings.Rankings {
			b.WriteString(fmt.Sprintf("%d. %s (%.1f)\n", r.Rank, r.Score.Code, r.Score.CompositeScore))
		}
		b.WriteString("\n")
	}

	b.WriteString("\nProvide a structured weekly outlook:\n")
	b.WriteString("1. MACRO THEME: Dominant narrative driving FX this week\n")
	b.WriteString("2. CURRENCY OUTLOOK: Bullish/Bearish bias for each G8 currency with reasons\n")
	b.WriteString("3. TOP TRADES: 3 highest-conviction pair trades with entry logic\n")
	b.WriteString("4. KEY RISKS: Events or scenarios that could invalidate the thesis\n")
	b.WriteString("5. CALENDAR FOCUS: Top 3 events to watch and expected market reaction\n")

	return b.String()
}

// BuildCrossMarketPrompt creates a prompt for cross-market COT analysis.
func BuildCrossMarketPrompt(cotData map[string]*domain.COTAnalysis) string {
	var b strings.Builder
	b.WriteString("Analyze cross-market COT positioning for intermarket signals.\n")
	b.WriteString("Look for correlations, divergences, and risk-on/risk-off signals.\n\n")

	for code, a := range cotData {
		if a == nil {
			continue
		}
		b.WriteString(fmt.Sprintf("%s (%s): SpecNet=%s COTIdx=%.0f Sentiment=%.0f Crowd=%.0f\n",
			code, a.Contract.Currency,
			fmtutil.FmtNumSigned(a.NetPosition, 0),
			a.COTIndex, a.SentimentScore, a.CrowdingIndex))
	}

	b.WriteString("\nProvide:\n")
	b.WriteString("1. Risk-on vs risk-off positioning assessment\n")
	b.WriteString("2. Safe-haven demand signals (JPY, CHF, USD)\n")
	b.WriteString("3. Commodity currency alignment (AUD, NZD, CAD)\n")
	b.WriteString("4. Any unusual cross-market divergences\n")

	return b.String()
}
