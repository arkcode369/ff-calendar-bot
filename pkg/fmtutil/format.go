// Package fmtutil provides number and text formatting helpers
// for Telegram message output.
package fmtutil

import (
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Number Formatting
// ---------------------------------------------------------------------------

// FmtNum formats a float64 with thousand separators and specified decimal places.
// Example: FmtNum(1234567.89, 2) => "1,234,567.89"
func FmtNum(v float64, decimals int) string {
	format := fmt.Sprintf("%%.%df", decimals)
	s := fmt.Sprintf(format, v)

	// Split on decimal point
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]

	// Handle negative
	neg := ""
	if strings.HasPrefix(intPart, "-") {
		neg = "-"
		intPart = intPart[1:]
	}

	// Add thousand separators
	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}

	out := neg + string(result)
	if len(parts) == 2 {
		out += "." + parts[1]
	}
	return out
}

// FmtNumSigned formats with a leading + or - sign.
// Example: FmtNumSigned(1234.5, 1) => "+1,234.5"
func FmtNumSigned(v float64, decimals int) string {
	if v > 0 {
		return "+" + FmtNum(v, decimals)
	}
	return FmtNum(v, decimals) // negative sign already included
}

// FmtPct formats a percentage with sign.
// Example: FmtPct(12.5) => "+12.5%"
func FmtPct(v float64) string {
	return FmtNumSigned(v, 1) + "%"
}

// FmtRatio formats a ratio with 2 decimal places.
// Example: FmtRatio(1.5) => "1.50"
func FmtRatio(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

// ---------------------------------------------------------------------------
// Visual Indicators
// ---------------------------------------------------------------------------

// COTIndexBar creates a visual bar for COT Index (0-100).
// Uses block characters to show positioning.
// Example: COTIndexBar(75) => "[=========-] 75"
func COTIndexBar(index float64, width int) string {
	if width <= 0 {
		width = 10
	}
	index = math.Max(0, math.Min(100, index))

	filled := int(math.Round(index / 100 * float64(width)))
	bar := strings.Repeat("=", filled) + strings.Repeat("-", width-filled)

	return fmt.Sprintf("[%s] %.0f", bar, index)
}

// ConfluenceBar creates a visual bar for Confluence Score (0-100).
// Shows direction: <30 bearish, 40-60 neutral, >70 bullish.
func ConfluenceBar(score float64) string {
	bar := COTIndexBar(score, 10)
	var label string
	switch {
	case score >= 70:
		label = "BULLISH"
	case score <= 30:
		label = "BEARISH"
	case score > 55:
		label = "LEAN BULL"
	case score < 45:
		label = "LEAN BEAR"
	default:
		label = "NEUTRAL"
	}
	return fmt.Sprintf("%s %s", bar, label)
}

// ImpactEmoji returns a text indicator for event impact level.
// High="!!!", Medium="!!", Low="!"
func ImpactEmoji(impact string) string {
	switch strings.ToLower(impact) {
	case "high":
		return "[!!!]"
	case "medium":
		return "[!!]"
	case "low":
		return "[!]"
	default:
		return "[-]"
	}
}

// DirectionArrow returns a text arrow for direction.
func DirectionArrow(value float64) string {
	if value > 0 {
		return ">>" // bullish/up
	}
	if value < 0 {
		return "<<" // bearish/down
	}
	return "--" // flat
}

// SignalLabel returns a human-readable signal label.
func SignalLabel(signal string) string {
	switch strings.ToUpper(signal) {
	case "BULLISH":
		return "[BULL]"
	case "BEARISH":
		return "[BEAR]"
	case "NEUTRAL":
		return "[NEUT]"
	case "EXTREME_BULL":
		return "[!!BULL!!]"
	case "EXTREME_BEAR":
		return "[!!BEAR!!]"
	default:
		return "[" + signal + "]"
	}
}

// ---------------------------------------------------------------------------
// Ranking Formatter
// ---------------------------------------------------------------------------

// RankMedal returns a position label for currency rankings.
// 1="#1", 2="#2", etc.
func RankMedal(position int) string {
	return fmt.Sprintf("#%d", position)
}

// RankBar creates a horizontal bar for ranking visualization.
// score is 0-100, maxWidth is the bar width in characters.
func RankBar(score float64, maxWidth int) string {
	if maxWidth <= 0 {
		maxWidth = 20
	}
	score = math.Max(0, math.Min(100, score))
	filled := int(math.Round(score / 100 * float64(maxWidth)))
	return strings.Repeat("|", filled) + strings.Repeat(".", maxWidth-filled)
}

// ---------------------------------------------------------------------------
// Text Helpers
// ---------------------------------------------------------------------------

// Truncate shortens a string to maxLen, appending "..." if truncated.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// PadRight pads a string to the specified width with spaces.
func PadRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// PadLeft pads a string to the specified width with spaces on the left.
func PadLeft(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// SectionHeader creates a formatted section header for Telegram messages.
// Example: SectionHeader("COT Analysis") => "=== COT ANALYSIS ==="
func SectionHeader(title string) string {
	return fmt.Sprintf("=== %s ===", strings.ToUpper(title))
}

// SubHeader creates a sub-section header.
// Example: SubHeader("Positioning") => "--- Positioning ---"
func SubHeader(title string) string {
	return fmt.Sprintf("--- %s ---", title)
}

// BulletList creates a bulleted list from items.
func BulletList(items []string) string {
	var sb strings.Builder
	for _, item := range items {
		sb.WriteString("  * ")
		sb.WriteString(item)
		sb.WriteByte('\n')
	}
	return sb.String()
}
