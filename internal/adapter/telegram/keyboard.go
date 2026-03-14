package telegram

import (
	"fmt"
	"strings"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
)

// ---------------------------------------------------------------------------
// KeyboardBuilder — Telegram inline keyboard construction
// ---------------------------------------------------------------------------

// KeyboardBuilder creates inline keyboards for interactive bot messages.
// All callback_data values use a prefix-based routing scheme:
//   - "cot:XXX"   -> COT detail for currency/contract
//   - "conf:XXX"  -> Confluence detail for pair
//   - "set:XXX"   -> Settings toggle action
//   - "alert:XXX" -> Alert action (mute, dismiss)
type KeyboardBuilder struct{}

// NewKeyboardBuilder creates a new KeyboardBuilder.
func NewKeyboardBuilder() *KeyboardBuilder {
	return &KeyboardBuilder{}
}

// ---------------------------------------------------------------------------
// COT Keyboards
// ---------------------------------------------------------------------------

// COTCurrencySelector builds a keyboard for selecting a currency to view COT detail.
// Layout: 2 currencies per row for the 8 major contracts.
func (kb *KeyboardBuilder) COTCurrencySelector(analyses []domain.COTAnalysis) ports.InlineKeyboard {
	var rows [][]ports.InlineButton
	var currentRow []ports.InlineButton

	for i, a := range analyses {
		// Extract short label from contract name
		label := kb.contractLabel(a.Contract.Name, a.Contract.Code)

		// Add bias indicator
		indicator := "--"
		if a.NetPosition > 0 {
			indicator = "+"
		} else if a.NetPosition < 0 {
			indicator = "-"
		}

		btn := ports.InlineButton{
			Text:         fmt.Sprintf("%s [%s]", label, indicator),
			CallbackData: fmt.Sprintf("cot:%s", a.Contract.Code),
		}
		currentRow = append(currentRow, btn)

		// 2 buttons per row
		if len(currentRow) == 2 || i == len(analyses)-1 {
			rows = append(rows, currentRow)
			currentRow = nil
		}
	}

	// Add cross-market row if we have gold/oil
	crossRow := kb.crossMarketRow(analyses)
	if len(crossRow) > 0 {
		rows = append(rows, crossRow)
	}

	return ports.InlineKeyboard{Rows: rows}
}

// crossMarketRow adds quick-access buttons for Gold and Oil if available.
func (kb *KeyboardBuilder) crossMarketRow(analyses []domain.COTAnalysis) []ports.InlineButton {
	var row []ports.InlineButton
	for _, a := range analyses {
		code := strings.ToUpper(a.Contract.Code)
		if code == "088691" || code == "067651" { // Gold, Oil
			continue // Already in main grid
		}
	}
	return row
}

// contractLabel extracts a short display label from a contract name.
func (kb *KeyboardBuilder) contractLabel(name, code string) string {
	// Common CFTC contract code to currency mappings
	labels := map[string]string{
		"099741": "EUR",
		"096742": "GBP",
		"097741": "JPY",
		"232741": "AUD",
		"112741": "NZD",
		"090741": "CAD",
		"092741": "CHF",
		"098662": "DXY",
		"088691": "GOLD",
		"067651": "OIL",
	}

	if label, ok := labels[code]; ok {
		return label
	}

	// Fallback: first word of contract name
	if parts := strings.Fields(name); len(parts) > 0 {
		if len(parts[0]) <= 5 {
			return strings.ToUpper(parts[0])
		}
		return strings.ToUpper(parts[0][:4])
	}

	return code
}

// ---------------------------------------------------------------------------
// Confluence Keyboards
// ---------------------------------------------------------------------------

// ConfluencePairSelector builds a keyboard for selecting a pair to view confluence detail.
// Layout: 2 pairs per row, sorted by absolute score (strongest first).
func (kb *KeyboardBuilder) ConfluencePairSelector(scores []domain.ConfluenceScore) ports.InlineKeyboard {
	var rows [][]ports.InlineButton
	var currentRow []ports.InlineButton

	for i, s := range scores {
		// Bias indicator
		indicator := "--"
		if s.TotalScore > 0.3 {
			indicator = "BUL"
		} else if s.TotalScore < -0.3 {
			indicator = "BER"
		} else {
			indicator = "NEU"
		}

		btn := ports.InlineButton{
			Text:         fmt.Sprintf("%s [%s]", s.CurrencyPair, indicator),
			CallbackData: fmt.Sprintf("conf:%s", s.CurrencyPair),
		}
		currentRow = append(currentRow, btn)

		if len(currentRow) == 2 || i == len(scores)-1 {
			rows = append(rows, currentRow)
			currentRow = nil
		}
	}

	return ports.InlineKeyboard{Rows: rows}
}

// ---------------------------------------------------------------------------
// Settings Keyboards
// ---------------------------------------------------------------------------

// SettingsMenu builds the settings control keyboard.
// Shows current state and toggle buttons for all preference options.
func (kb *KeyboardBuilder) SettingsMenu(prefs domain.UserPrefs) ports.InlineKeyboard {
	var rows [][]ports.InlineButton

	// Row 1: Alert toggle
	alertLabel := "Alerts: OFF -> Turn ON"
	if prefs.AlertsEnabled {
		alertLabel = "Alerts: ON -> Turn OFF"
	}
	rows = append(rows, []ports.InlineButton{{
		Text:         alertLabel,
		CallbackData: "set:alerts_toggle",
	}})

	// Row 2: AI Reports toggle
	aiLabel := "AI Reports: OFF -> Turn ON"
	if prefs.AIReportsEnabled {
		aiLabel = "AI Reports: ON -> Turn OFF"
	}
	rows = append(rows, []ports.InlineButton{{
		Text:         aiLabel,
		CallbackData: "set:ai_toggle",
	}})

	// Row 3: Impact level presets
	rows = append(rows, []ports.InlineButton{
		{Text: "High Only", CallbackData: "set:impact_high_only"},
		{Text: "High+Med", CallbackData: "set:impact_high_med"},
		{Text: "All", CallbackData: "set:impact_all"},
	})

	// Row 4: Timing presets
	rows = append(rows, []ports.InlineButton{
		{Text: "60/15/5 min", CallbackData: "set:time_60_15_5"},
		{Text: "15/5 min", CallbackData: "set:time_15_5"},
		{Text: "5/1 min", CallbackData: "set:time_5_1"},
	})

	return ports.InlineKeyboard{Rows: rows}
}

// ---------------------------------------------------------------------------
// Alert Action Keyboards
// ---------------------------------------------------------------------------

// AlertActions builds action buttons for an alert notification.
// Provides quick mute and dismiss options.
func (kb *KeyboardBuilder) AlertActions() ports.InlineKeyboard {
	return ports.InlineKeyboard{
		Rows: [][]ports.InlineButton{
			{
				{Text: "Mute Alerts", CallbackData: "alert:mute_1h"},
				{Text: "Dismiss", CallbackData: "alert:dismiss"},
			},
		},
	}
}

// EventDetailLink builds a keyboard with a link to the ForexFactory event page.
func (kb *KeyboardBuilder) EventDetailLink(event domain.FFEvent) ports.InlineKeyboard {
	var rows [][]ports.InlineButton

	if event.DetailURL != "" {
		rows = append(rows, []ports.InlineButton{{
			Text: "View on ForexFactory",
			URL:  event.DetailURL,
		}})
	}

	if event.SourceURL != "" {
		rows = append(rows, []ports.InlineButton{{
			Text: "Source",
			URL:  event.SourceURL,
		}})
	}

	return ports.InlineKeyboard{Rows: rows}
}

// ---------------------------------------------------------------------------
// Navigation Keyboards
// ---------------------------------------------------------------------------

// BackToOverview builds a "Back" button that triggers the overview callback.
func (kb *KeyboardBuilder) BackToOverview(section string) ports.InlineKeyboard {
	return ports.InlineKeyboard{
		Rows: [][]ports.InlineButton{
			{
				{Text: "<< Back to Overview", CallbackData: fmt.Sprintf("%s:overview", section)},
			},
		},
	}
}

// MainMenu builds a quick-access keyboard for the main bot features.
func (kb *KeyboardBuilder) MainMenu() ports.InlineKeyboard {
	return ports.InlineKeyboard{
		Rows: [][]ports.InlineButton{
			{
				{Text: "Today", CallbackData: "nav:today"},
				{Text: "Week", CallbackData: "nav:week"},
			},
			{
				{Text: "COT", CallbackData: "nav:cot"},
				{Text: "Rank", CallbackData: "nav:rank"},
			},
			{
				{Text: "Confluence", CallbackData: "nav:confluence"},
				{Text: "Outlook", CallbackData: "nav:outlook"},
			},
		},
	}
}
