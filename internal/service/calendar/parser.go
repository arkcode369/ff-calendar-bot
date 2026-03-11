package calendar

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// Parser handles HTML parsing of ForexFactory calendar pages.
// It extracts events from the weekly calendar table and historical
// data from individual event detail pages.
type Parser struct {
	loc *time.Location
}

// NewParser creates a parser that interprets FF times as ET,
// then converts to WIB for internal storage.
func NewParser() *Parser {
	return &Parser{
		loc: timeutil.WIB(),
	}
}

var (
	// Regex patterns for FF calendar HTML parsing
	reImpactClass  = regexp.MustCompile(`icon--ff-impact-(red|ora|yel|gra)`)
	reEventID      = regexp.MustCompile(`data-eventid="(\d+)"`)
	reDateTime     = regexp.MustCompile(`data-time="(\d+)"`)
	reNumericValue = regexp.MustCompile(`^[+-]?[\d,]+\.?\d*[%KMBTkmbt]?$`)
	reSpeakerTag   = regexp.MustCompile(`(?i)(speaks?|speech|testimony|conference|remarks)`)
	reAllDay       = regexp.MustCompile(`(?i)(all\s*day|tentative)`)
	reHistoryRow   = regexp.MustCompile(`<tr[^>]*class="[^"]*calendar_row[^"]*"[^>]*>`)
	reHTMLTag      = regexp.MustCompile(`<[^>]+>`)
)

// impactMap converts FF CSS class suffixes to domain impact levels.
var impactMap = map[string]domain.EventImpactLevel{
	"red": domain.ImpactHigh,
	"ora": domain.ImpactMedium,
	"yel": domain.ImpactLow,
	"gra": domain.ImpactHoliday,
}

// ParseWeeklyCalendarHTML extracts events from the FF weekly calendar HTML.
// The HTML is a <table> with class "calendar__table" containing rows
// where each row has: date, time, currency, impact, event name, actual, forecast, previous.
func (p *Parser) ParseWeeklyCalendarHTML(html string) ([]domain.FFEvent, error) {
	if html == "" {
		return nil, fmt.Errorf("empty HTML")
	}

	rows := splitCalendarRows(html)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no calendar rows found")
	}

	var events []domain.FFEvent
	var currentDate time.Time

	for _, row := range rows {
		// Extract date if present (date rows span the full week)
		if d := extractDate(row); !d.IsZero() {
			currentDate = d
			continue
		}

		ev, err := p.parseEventRow(row, currentDate)
		if err != nil {
			continue // skip unparseable rows
		}

		events = append(events, ev)
	}

	return events, nil
}

// parseEventRow extracts a single event from an HTML table row.
func (p *Parser) parseEventRow(row string, date time.Time) (domain.FFEvent, error) {
	ev := domain.FFEvent{}

	// Event ID
	if m := reEventID.FindStringSubmatch(row); len(m) > 1 {
		ev.ID = m[1]
	}

	// Time
	ev.IsAllDay = reAllDay.MatchString(row)
	if !ev.IsAllDay {
		if m := reDateTime.FindStringSubmatch(row); len(m) > 1 {
			ts, _ := strconv.ParseInt(m[1], 10, 64)
			if ts > 0 {
				// FF uses Unix timestamp in ET
				ev.DateTime = time.Unix(ts, 0).In(p.loc)
			}
		}
	}
	if ev.DateTime.IsZero() && !date.IsZero() {
		ev.DateTime = date
	}

	// Currency
	ev.Currency = extractBetween(row, `class="calendar__currency">`, `</`)
	ev.Currency = strings.TrimSpace(ev.Currency)

	// Impact
	if m := reImpactClass.FindStringSubmatch(row); len(m) > 1 {
		ev.Impact = impactMap[m[1]]
	}

	// Event name
	ev.Title = cleanHTML(extractBetween(row, `class="calendar__event-title">`, `</`))
	ev.Title = strings.TrimSpace(ev.Title)
	if ev.Title == "" {
		return ev, fmt.Errorf("no event title")
	}

	// Actual, Forecast, Previous values
	ev.Actual = cleanHTML(extractBetween(row, `class="calendar__actual">`, `</`))
	ev.Forecast = cleanHTML(extractBetween(row, `class="calendar__forecast">`, `</`))
	ev.Previous = cleanHTML(extractBetween(row, `class="calendar__previous">`, `</`))

	// Source URL for history scraping
	ev.SourceURL = extractHref(row, "calendar__event-title")

	// Detect speaker events
	if reSpeakerTag.MatchString(ev.Title) {
		ev.SpeakerName = extractSpeakerName(ev.Title)
	}

	// Detect preliminary flag
	ev.IsPreliminary = strings.Contains(strings.ToLower(row), "preliminary") ||
		strings.Contains(strings.ToLower(row), "flash")

	return ev, nil
}

// ParseEventHistoryHTML extracts historical data points from an event detail page.
// Returns up to 24 months of Actual/Forecast/Previous data for surprise calculation.
func (p *Parser) ParseEventHistoryHTML(html string) ([]domain.FFEventDetail, error) {
	if html == "" {
		return nil, fmt.Errorf("empty HTML")
	}

	rows := reHistoryRow.FindAllString(html, -1)
	var details []domain.FFEventDetail

	for _, row := range rows {
		d := p.parseHistoryRow(row)
		if d.Date.IsZero() {
			continue
		}
		details = append(details, d)
	}

	// Limit to 24 months max
	if len(details) > 24 {
		details = details[:24]
	}

	return details, nil
}

// parseHistoryRow extracts a single historical data point.
func (p *Parser) parseHistoryRow(row string) domain.FFEventDetail {
	d := domain.FFEventDetail{}

	// Date extraction from history table
	dateStr := cleanHTML(extractBetween(row, `class="calendar__date">`, `</`))
	dateStr = strings.TrimSpace(dateStr)
	if t, err := time.Parse("Jan 02, 2006", dateStr); err == nil {
		d.Date = t
	} else if t, err := time.Parse("Jan 2, 2006", dateStr); err == nil {
		d.Date = t
	}

	d.Actual = cleanNumeric(extractBetween(row, `class="calendar__actual">`, `</`))
	d.Forecast = cleanNumeric(extractBetween(row, `class="calendar__forecast">`, `</`))
	d.Previous = cleanNumeric(extractBetween(row, `class="calendar__previous">`, `</`))

	// Mark preliminary/revised data
	rowLower := strings.ToLower(row)
	d.IsPreliminary = strings.Contains(rowLower, "preliminary") || strings.Contains(rowLower, "flash")
	d.IsRevised = strings.Contains(rowLower, "revised")

	return d
}

// --- HTML utility helpers ---

// splitCalendarRows splits the calendar HTML into individual table rows.
func splitCalendarRows(html string) []string {
	var rows []string
	parts := strings.Split(html, "<tr")
	for _, part := range parts[1:] { // skip content before first <tr
		idx := strings.Index(part, "</tr>")
		if idx > 0 {
			rows = append(rows, "<tr"+part[:idx+5])
		}
	}
	return rows
}

// extractDate tries to parse a date from a date-separator row.
func extractDate(row string) time.Time {
	// FF date rows have class "calendar__row--day-breaker"
	if !strings.Contains(row, "day-breaker") && !strings.Contains(row, "calendar__date") {
		return time.Time{}
	}

	dateStr := cleanHTML(extractBetween(row, `class="calendar__date">`, `</`))
	dateStr = strings.TrimSpace(dateStr)

	// FF uses formats like "Mon Mar 10" (no year)
	now := time.Now()
	year := now.Year()

	formats := []string{
		"Mon Jan 2",
		"Monday, January 2",
		"Jan 2",
	}

	for _, fmt := range formats {
		if t, err := time.Parse(fmt, dateStr); err == nil {
			return time.Date(year, t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		}
	}

	return time.Time{}
}

// extractBetween extracts text between a start marker and end marker.
func extractBetween(html, startMarker, endMarker string) string {
	idx := strings.Index(html, startMarker)
	if idx < 0 {
		return ""
	}
	start := idx + len(startMarker)
	end := strings.Index(html[start:], endMarker)
	if end < 0 {
		return ""
	}
	return html[start : start+end]
}

// extractHref finds the href in a link near the given class.
func extractHref(html, nearClass string) string {
	idx := strings.Index(html, nearClass)
	if idx < 0 {
		return ""
	}

	// Search backwards for href
	chunk := html[:idx]
	hrefIdx := strings.LastIndex(chunk, `href="`)
	if hrefIdx < 0 {
		// Search forwards
		chunk = html[idx:]
		hrefIdx = strings.Index(chunk, `href="`)
		if hrefIdx < 0 {
			return ""
		}
		chunk = chunk[hrefIdx:]
	} else {
		chunk = chunk[hrefIdx:]
	}

	start := strings.Index(chunk, `"`) + 1
	end := strings.Index(chunk[start:], `"`)
	if end < 0 {
		return ""
	}

	url := chunk[start : start+end]
	if strings.HasPrefix(url, "/") {
		return "https://www.forexfactory.com" + url
	}
	return url
}

// extractSpeakerName tries to extract a person's name from an event title.
// e.g., "Fed Chair Powell Speaks" -> "Powell"
func extractSpeakerName(title string) string {
	words := strings.Fields(title)
	for i, w := range words {
		if reSpeakerTag.MatchString(w) && i > 0 {
			return words[i-1]
		}
	}
	// Fallback: second word is usually the name
	if len(words) >= 2 {
		return words[len(words)-2]
	}
	return ""
}

// cleanHTML strips HTML tags from a string.
func cleanHTML(s string) string {
	return strings.TrimSpace(reHTMLTag.ReplaceAllString(s, ""))
}

// cleanNumeric extracts numeric value, stripping HTML and whitespace.
func cleanNumeric(s string) string {
	s = cleanHTML(s)
	s = strings.TrimSpace(s)
	if reNumericValue.MatchString(s) {
		return s
	}
	// Try extracting just the number
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	return s
}
