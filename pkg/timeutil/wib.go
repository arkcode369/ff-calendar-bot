// Package timeutil provides timezone and time formatting helpers
// with WIB (Western Indonesia Time, UTC+7) as the primary timezone.
package timeutil

import (
	"fmt"
	"time"
)

// WIB is the Asia/Jakarta timezone location.
var WIB *time.Location

func init() {
	var err error
	WIB, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// Fallback: fixed UTC+7 offset
		WIB = time.FixedZone("WIB", 7*60*60)
	}
}

// NowWIB returns the current time in WIB.
func NowWIB() time.Time {
	return time.Now().In(WIB)
}

// ToWIB converts any time.Time to WIB.
func ToWIB(t time.Time) time.Time {
	return t.In(WIB)
}

// TodayWIB returns today's date at midnight in WIB.
func TodayWIB() time.Time {
	now := NowWIB()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, WIB)
}

// StartOfWeekWIB returns the Monday of the week containing t in WIB.
func StartOfWeekWIB(t time.Time) time.Time {
	w := t.In(WIB)
	weekday := int(w.Weekday())
	if weekday == 0 {
		weekday = 7 // Sunday = 7
	}
	monday := w.AddDate(0, 0, -(weekday - 1))
	return time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, WIB)
}

// StartOfWeek is an alias for StartOfWeekWIB.
func StartOfWeek(t time.Time) time.Time {
	return StartOfWeekWIB(t)
}

// EndOfWeekWIB returns the Sunday 23:59:59 of the week containing t in WIB.
func EndOfWeekWIB() time.Time {
	monday := StartOfWeekWIB(NowWIB())
	sunday := monday.AddDate(0, 0, 6)
	return time.Date(sunday.Year(), sunday.Month(), sunday.Day(), 23, 59, 59, 0, WIB)
}

// StartOfDayWIB returns the start of the day (00:00:00) for the given time in WIB.
func StartOfDayWIB(t time.Time) time.Time {
	w := t.In(WIB)
	return time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, WIB)
}

// StartOfDay is an alias for StartOfDayWIB.
func StartOfDay(t time.Time) time.Time {
	return StartOfDayWIB(t)
}

// EndOfDay returns the end of the day (23:59:59) for the given time in WIB.
func EndOfDay(t time.Time) time.Time {
	w := t.In(WIB)
	return time.Date(w.Year(), w.Month(), w.Day(), 23, 59, 59, 0, WIB)
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// FormatDate formats time as "Mon 02 Jan" in WIB.
func FormatDate(t time.Time) string {
	return ToWIB(t).Format("Mon 02 Jan")
}

// FormatDateTime formats time as "Mon 02 Jan 15:04 WIB".
func FormatDateTime(t time.Time) string {
	return ToWIB(t).Format("Mon 02 Jan 15:04") + " WIB"
}

// FormatTime formats time as "15:04 WIB".
func FormatTime(t time.Time) string {
	return ToWIB(t).Format("15:04") + " WIB"
}

// FormatDateISO formats time as "2006-01-02".
func FormatDateISO(t time.Time) string {
	return ToWIB(t).Format("2006-01-02")
}

// FormatDateTimeISO formats time as "2006-01-02T15:04:05".
func FormatDateTimeISO(t time.Time) string {
	return ToWIB(t).Format("2006-01-02T15:04:05")
}

// FormatTimestamp formats time as Unix timestamp string.
func FormatTimestamp(t time.Time) string {
	return fmt.Sprintf("%d", t.Unix())
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// ParseDateISO parses "2006-01-02" string in WIB timezone.
func ParseDateISO(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, WIB)
}

// ParseDateTimeISO parses "2006-01-02T15:04:05" string in WIB timezone.
func ParseDateTimeISO(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02T15:04:05", s, WIB)
}

// ParseFFDate parses ForexFactory date formats.
// Handles: "Mon Jan 2" (current year assumed), "Jan 2", "2006-01-02".
func ParseFFDate(s string) (time.Time, error) {
	now := NowWIB()

	// Try ISO format first
	if t, err := ParseDateISO(s); err == nil {
		return t, nil
	}

	// Try "Mon Jan 2" (e.g., "Tue Mar 11")
	if t, err := time.ParseInLocation("Mon Jan 2", s, WIB); err == nil {
		return time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, WIB), nil
	}

	// Try "Jan 2" (e.g., "Mar 11")
	if t, err := time.ParseInLocation("Jan 2", s, WIB); err == nil {
		return time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, WIB), nil
	}

	return time.Time{}, fmt.Errorf("timeutil: cannot parse FF date %q", s)
}

// ParseFFTime parses ForexFactory time like "8:30am", "2:00pm", "All Day", "Tentative".
// Returns the time component and a boolean indicating if it's a valid time (not All Day/Tentative).
func ParseFFTime(s string) (hour, minute int, valid bool) {
	if s == "" || s == "All Day" || s == "Tentative" || s == "Day 1" || s == "Day 2" {
		return 0, 0, false
	}

	var h, m int
	var ampm string
	_, err := fmt.Sscanf(s, "%d:%d%s", &h, &m, &ampm)
	if err != nil {
		return 0, 0, false
	}

	if ampm == "pm" && h != 12 {
		h += 12
	}
	if ampm == "am" && h == 12 {
		h = 0
	}

	return h, m, true
}

// ---------------------------------------------------------------------------
// Duration Helpers
// ---------------------------------------------------------------------------

// MinutesUntil returns the number of minutes between now (WIB) and target.
func MinutesUntil(target time.Time) int {
	return int(time.Until(target).Minutes())
}

// DaysAgo returns a time.Time that is n days before now in WIB.
func DaysAgo(n int) time.Time {
	return NowWIB().AddDate(0, 0, -n)
}

// WeeksAgo returns a time.Time that is n weeks before now in WIB.
func WeeksAgo(n int) time.Time {
	return NowWIB().AddDate(0, 0, -n*7)
}

// IsSameDay checks if two times fall on the same calendar day in WIB.
func IsSameDay(a, b time.Time) bool {
	aw := ToWIB(a)
	bw := ToWIB(b)
	return aw.Year() == bw.Year() && aw.YearDay() == bw.YearDay()
}

// IsWeekend returns true if the given time falls on Saturday or Sunday in WIB.
func IsWeekend(t time.Time) bool {
	wd := ToWIB(t).Weekday()
	return wd == time.Saturday || wd == time.Sunday
}
