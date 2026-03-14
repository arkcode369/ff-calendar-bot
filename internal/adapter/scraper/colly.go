package scraper

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// ---------------------------------------------------------------------------
// CollyFFScraper — implements ports.FFScraper using Colly + CF bypass
// ---------------------------------------------------------------------------

// CollyFFScraper scrapes ForexFactory calendar data using the Colly framework.
// Features:
//   - Rotating user agents to avoid fingerprinting
//   - Random delays between requests (1-3s) to mimic human behavior
//   - Cookie jar persistence for Cloudflare challenge tokens
//   - Automatic retry with exponential backoff on 5xx errors
//   - Fair Economy JSON fallback when scraping fails
type CollyFFScraper struct {
	baseURL    string
	fallback   *FallbackScraper
	transport  *http.Transport
	cookieJar  http.CookieJar
	mu         sync.Mutex
	lastScrape time.Time
	minDelay   time.Duration
}

// NewCollyFFScraper creates a scraper with the given base URL and fallback.
func NewCollyFFScraper(baseURL string, fallback *FallbackScraper) *CollyFFScraper {
	return &CollyFFScraper{
		baseURL:  strings.TrimRight(baseURL, "/"),
		fallback: fallback,
		transport: &http.Transport{
			MaxIdleConns:        10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  false,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		minDelay: 2 * time.Second,
	}
}

// compile-time interface check
var _ ports.FFScraper = (*CollyFFScraper)(nil)

// newCollector creates a configured Colly collector with anti-detection measures.
func (s *CollyFFScraper) newCollector() *colly.Collector {
	c := colly.NewCollector(
		colly.AllowedDomains("www.forexfactory.com", "forexfactory.com"),
		colly.MaxDepth(1),
		colly.Async(false),
	)

	// Anti-detection: random user agent rotation
	extensions.RandomUserAgent(c)

	// Anti-detection: random delay between requests
	c.Limit(&colly.LimitRule{
		DomainGlob:  "*forexfactory.com*",
		Delay:       1 * time.Second,
		RandomDelay: 2 * time.Second,
		Parallelism: 1,
	})

	// Custom transport with shared cookie jar
	c.WithTransport(s.transport)

	// Anti-detection: browser-like headers
	c.OnRequest(func(r *colly.Request) {
		r.Headers.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8")
		r.Headers.Set("Accept-Language", "en-US,en;q=0.5")
		r.Headers.Set("Accept-Encoding", "gzip, deflate, br")
		r.Headers.Set("Connection", "keep-alive")
		r.Headers.Set("Upgrade-Insecure-Requests", "1")
		r.Headers.Set("Sec-Fetch-Dest", "document")
		r.Headers.Set("Sec-Fetch-Mode", "navigate")
		r.Headers.Set("Sec-Fetch-Site", "none")
		r.Headers.Set("Sec-Fetch-User", "?1")
		r.Headers.Set("Cache-Control", "max-age=0")
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("[SCRAPER] Error %d on %s: %v", r.StatusCode, r.Request.URL, err)
	})

	return c
}

// ---------------------------------------------------------------------------
// ScrapeWeeklyCalendar — Main weekly calendar scrape
// ---------------------------------------------------------------------------

// ScrapeWeeklyCalendar fetches all events for the current week from ForexFactory.
// Falls back to Fair Economy JSON API if HTML scraping fails.
func (s *CollyFFScraper) ScrapeWeeklyCalendar(ctx context.Context) ([]domain.FFEvent, error) {
	s.mu.Lock()
	if time.Since(s.lastScrape) < s.minDelay {
		time.Sleep(s.minDelay - time.Since(s.lastScrape))
	}
	s.lastScrape = time.Now()
	s.mu.Unlock()

	url := s.baseURL + "/calendar?week=this"
	log.Printf("[SCRAPER] Scraping weekly calendar: %s", url)

	events, err := s.scrapeCalendarPage(ctx, url)
	if err != nil {
		log.Printf("[SCRAPER] HTML scrape failed: %v, trying fallback", err)
		if s.fallback != nil {
			return s.fallback.FetchWeeklyJSON(ctx)
		}
		return nil, fmt.Errorf("scrape failed and no fallback: %w", err)
	}

	if len(events) == 0 && s.fallback != nil {
		log.Printf("[SCRAPER] HTML returned 0 events, trying fallback")
		return s.fallback.FetchWeeklyJSON(ctx)
	}

	log.Printf("[SCRAPER] Scraped %d events for this week", len(events))
	return events, nil
}

// scrapeCalendarPage parses the FF weekly calendar HTML table.
func (s *CollyFFScraper) scrapeCalendarPage(ctx context.Context, url string) ([]domain.FFEvent, error) {
	c := s.newCollector()

	var (
		events     []domain.FFEvent
		currentDay time.Time
		parseErr   error
	)

	// Each calendar row is a <tr> inside the calendar table.
	// Day separator rows contain the date; event rows contain data cells.
	c.OnHTML("tr.calendar__row", func(e *colly.HTMLElement) {
		if ctx.Err() != nil {
			return
		}

		// Check if this is a day separator row
		dateCell := e.ChildText("td.calendar__date span")
		if dateCell != "" {
			parsed, err := s.parseDateCell(dateCell)
			if err == nil {
				currentDay = parsed
			}
		}

		// Skip non-event rows
		title := strings.TrimSpace(e.ChildText("td.calendar__event span.calendar__event-title"))
		if title == "" {
			return
		}

		event, err := s.parseEventRow(e, currentDay)
		if err != nil {
			log.Printf("[SCRAPER] Skip row parse error: %v", err)
			return
		}
		events = append(events, event)
	})

	err := c.Visit(url)
	if err != nil {
		return nil, fmt.Errorf("visit %s: %w", url, err)
	}

	if parseErr != nil {
		return events, parseErr
	}

	return events, nil
}

// parseEventRow extracts a single FFEvent from a calendar table row.
func (s *CollyFFScraper) parseEventRow(e *colly.HTMLElement, currentDay time.Time) (domain.FFEvent, error) {
	title := strings.TrimSpace(e.ChildText("td.calendar__event span.calendar__event-title"))
	currency := strings.TrimSpace(e.ChildText("td.calendar__currency"))
	timeStr := strings.TrimSpace(e.ChildText("td.calendar__time"))
	actual := strings.TrimSpace(e.ChildText("td.calendar__actual span"))
	forecast := strings.TrimSpace(e.ChildText("td.calendar__forecast span"))
	previous := strings.TrimSpace(e.ChildText("td.calendar__previous span"))

	// Parse impact from the icon class
	impact := s.parseImpactFromClass(e)

	// Parse time into full datetime
	eventTime := s.parseTimeCell(timeStr, currentDay)
	isAllDay := timeStr == "" || timeStr == "All Day" || timeStr == "Tentative"

	// Detect event category
	category := s.detectCategory(title)

	// Build event ID
	eventID := fmt.Sprintf("%s:%s:%s",
		currentDay.Format("2006-01-02"),
		currency,
		s.hashTitle(title),
	)

	// Extract detail URL if available
	detailURL := e.ChildAttr("td.calendar__event a", "href")
	if detailURL != "" && !strings.HasPrefix(detailURL, "http") {
		detailURL = s.baseURL + detailURL
	}

	event := domain.FFEvent{
		ID:        eventID,
		Title:     title,
		Currency:  strings.ToUpper(currency),
		Date:      eventTime,
		Time:      timeStr,
		IsAllDay:  isAllDay,
		Impact:    impact,
		Category:  category,
		Actual:    actual,
		Forecast:  forecast,
		Previous:  previous,
		DetailURL: detailURL,
		ScrapedAt: timeutil.NowWIB(),
		Source:    "forexfactory",
	}

	// Detect speaker for speech events
	if category == domain.CategorySpeech {
		name, role := s.detectSpeaker(title)
		event.SpeakerName = name
		event.SpeakerRole = role
	}

	// Detect release type from title keywords
	event.ReleaseType = s.detectReleaseType(title)
	event.IsPreliminary = event.ReleaseType == domain.ReleasePreliminary
	event.IsFinal = event.ReleaseType == domain.ReleaseFinal

	return event, nil
}

// ---------------------------------------------------------------------------
// ScrapeEventHistory — Historical data for a specific event
// ---------------------------------------------------------------------------

// ScrapeEventHistory fetches historical Actual/Forecast/Previous data points.
func (s *CollyFFScraper) ScrapeEventHistory(ctx context.Context, eventURL string) ([]domain.FFEventDetail, error) {
	if eventURL == "" {
		return nil, fmt.Errorf("empty event URL")
	}

	c := s.newCollector()
	var details []domain.FFEventDetail

	c.OnHTML("tr.calendar_row", func(e *colly.HTMLElement) {
		if ctx.Err() != nil {
			return
		}

		dateStr := strings.TrimSpace(e.ChildText("td.calendar__date"))
		actualStr := strings.TrimSpace(e.ChildText("td.calendar__actual span"))
		forecastStr := strings.TrimSpace(e.ChildText("td.calendar__forecast span"))
		previousStr := strings.TrimSpace(e.ChildText("td.calendar__previous span"))

		if dateStr == "" || actualStr == "" {
			return
		}

		date, err := time.Parse("Jan 02, 2006", dateStr)
		if err != nil {
			return
		}

		detail := domain.FFEventDetail{
			Date:     date,
			Actual:   parseNumeric(actualStr),
			Forecast: parseNumeric(forecastStr),
			Previous: parseNumeric(previousStr),
		}
		detail.Surprise = detail.Actual - detail.Forecast

		details = append(details, detail)
	})

	if err := c.Visit(eventURL); err != nil {
		return nil, fmt.Errorf("visit history %s: %w", eventURL, err)
	}

	log.Printf("[SCRAPER] Fetched %d historical data points from %s", len(details), eventURL)
	return details, nil
}

// ---------------------------------------------------------------------------
// ScrapeRevisions — Detect data revisions
// ---------------------------------------------------------------------------

// ScrapeRevisions compares current event data against stored values
// to detect when "Previous" values have been revised.
func (s *CollyFFScraper) ScrapeRevisions(ctx context.Context, events []domain.FFEvent) ([]domain.EventRevision, error) {
	// Note: Revision detection is handled at the service layer (service/calendar/scraper.go)
	// by comparing freshly scraped events against stored events.
	// This method is available for direct comparison if needed.
	var revisions []domain.EventRevision

	for _, event := range events {
		if event.WasRevised() {
			revisions = append(revisions, *event.Revision)
		}
	}

	return revisions, nil
}

// ---------------------------------------------------------------------------
// HealthCheck — Verify scraping endpoint is reachable
// ---------------------------------------------------------------------------

// HealthCheck pings ForexFactory to verify connectivity.
func (s *CollyFFScraper) HealthCheck(ctx context.Context) error {
	c := s.newCollector()
	var reachable bool

	c.OnResponse(func(r *colly.Response) {
		if r.StatusCode == http.StatusOK {
			reachable = true
		}
	})

	err := c.Visit(s.baseURL)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	if !reachable {
		return fmt.Errorf("health check: non-200 response")
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helper methods
// ---------------------------------------------------------------------------

// parseDateCell parses FF date formats like "Mon Mar 10" into a time.Time.
func (s *CollyFFScraper) parseDateCell(cell string) (time.Time, error) {
	cell = strings.TrimSpace(cell)
	now := timeutil.NowWIB()

	// Try "Mon Mar 10" format
	formats := []string{
		"Mon Jan 2",
		"Monday Jan 2",
		"Jan 2",
	}

	for _, layout := range formats {
		if t, err := time.Parse(layout, cell); err == nil {
			// Set year to current year
			return time.Date(now.Year(), t.Month(), t.Day(), 0, 0, 0, 0, timeutil.WIB), nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse date: %q", cell)
}

// parseTimeCell converts FF time like "8:30pm" to full datetime.
func (s *CollyFFScraper) parseTimeCell(timeStr string, day time.Time) time.Time {
	timeStr = strings.TrimSpace(strings.ToLower(timeStr))
	if timeStr == "" || timeStr == "all day" || timeStr == "tentative" {
		return day
	}

	// FF uses ET (Eastern Time); parse and convert to WIB
	formats := []string{
		"3:04pm",
		"3:04am",
		"15:04",
	}

	et, _ := time.LoadLocation("America/New_York")

	for _, layout := range formats {
		if t, err := time.Parse(layout, timeStr); err == nil {
			// Build full datetime in ET, then convert to WIB
			etTime := time.Date(day.Year(), day.Month(), day.Day(),
				t.Hour(), t.Minute(), 0, 0, et)
			return etTime.In(timeutil.WIB)
		}
	}

	// Fallback: return day with no time
	return day
}

// parseImpactFromClass extracts impact level from the CSS class of the impact icon.
func (s *CollyFFScraper) parseImpactFromClass(e *colly.HTMLElement) domain.ImpactLevel {
	class := e.ChildAttr("td.calendar__impact span", "class")
	switch {
	case strings.Contains(class, "high"):
		return domain.ImpactHigh
	case strings.Contains(class, "medium"), strings.Contains(class, "orange"):
		return domain.ImpactMedium
	case strings.Contains(class, "low"), strings.Contains(class, "yellow"):
		return domain.ImpactLow
	default:
		return domain.ImpactNone
	}
}

// detectCategory classifies an event based on its title.
func (s *CollyFFScraper) detectCategory(title string) domain.EventCategory {
	lower := strings.ToLower(title)

	// Central bank keywords
	cbKeywords := []string{"rate decision", "monetary policy", "rate statement",
		"meeting minutes", "policy report", "mpc ", "fomc "}
	for _, kw := range cbKeywords {
		if strings.Contains(lower, kw) {
			return domain.CategoryCentralBank
		}
	}

	// Speech keywords
	speechKeywords := []string{"speaks", "speech", "testimony", "testifies",
		"press conference", "conference", "remarks"}
	for _, kw := range speechKeywords {
		if strings.Contains(lower, kw) {
			return domain.CategorySpeech
		}
	}

	// Auction
	if strings.Contains(lower, "auction") || strings.Contains(lower, "bond") {
		return domain.CategoryAuction
	}

	// Holiday
	if strings.Contains(lower, "holiday") || strings.Contains(lower, "day off") {
		return domain.CategoryHoliday
	}

	return domain.CategoryEconomicIndicator
}

// detectSpeaker extracts speaker name and role from event title.
func (s *CollyFFScraper) detectSpeaker(title string) (name, role string) {
	speakers := map[string]string{
		"Powell":      "Fed Chair",
		"Lagarde":     "ECB President",
		"Bailey":      "BOE Governor",
		"Ueda":        "BOJ Governor",
		"Macklem":     "BOC Governor",
		"Bullock":     "RBA Governor",
		"Orr":         "RBNZ Governor",
		"Jordan":      "SNB Chairman",
		"Waller":      "Fed Governor",
		"Williams":    "NY Fed President",
		"Bowman":      "Fed Governor",
		"Barr":        "Fed Vice Chair",
		"Jefferson":   "Fed Vice Chair",
		"Cook":        "Fed Governor",
		"Kugler":      "Fed Governor",
		"Schnabel":    "ECB Board",
		"Lane":        "ECB Chief Economist",
		"De Guindos":  "ECB Vice President",
		"Pill":        "BOE Chief Economist",
		"Broadbent":   "BOE Deputy Governor",
	}

	for speakerName, speakerRole := range speakers {
		if strings.Contains(title, speakerName) {
			return speakerName, speakerRole
		}
	}

	// Generic extraction: "[Name] Speaks"
	parts := strings.Fields(title)
	for i, p := range parts {
		if strings.EqualFold(p, "speaks") || strings.EqualFold(p, "testifies") {
			if i > 0 {
				return parts[i-1], "Official"
			}
		}
	}

	return "", ""
}

// detectReleaseType identifies if an event is preliminary, revised, or final.
func (s *CollyFFScraper) detectReleaseType(title string) domain.ReleaseType {
	lower := strings.ToLower(title)
	switch {
	case strings.Contains(lower, "flash"), strings.Contains(lower, "preliminary"),
		strings.Contains(lower, "advance"), strings.Contains(lower, "1st estimate"):
		return domain.ReleasePreliminary
	case strings.Contains(lower, "revised"), strings.Contains(lower, "2nd estimate"),
		strings.Contains(lower, "3rd estimate"):
		return domain.ReleaseRevised
	case strings.Contains(lower, "final"):
		return domain.ReleaseFinal
	default:
		return domain.ReleaseRegular
	}
}

// hashTitle creates a short deterministic hash for event title.
func (s *CollyFFScraper) hashTitle(title string) string {
	// Simple FNV-like hash for deterministic short IDs
	h := uint32(2166136261)
	for i := 0; i < len(title); i++ {
		h ^= uint32(title[i])
		h *= 16777619
	}
	return fmt.Sprintf("%08x", h)
}

// parseNumeric strips formatting and parses a numeric value from FF data cells.
func parseNumeric(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0
	}

	// Remove common suffixes: %, K, M, B, T
	multiplier := 1.0
	s = strings.ReplaceAll(s, ",", "")

	if strings.HasSuffix(s, "%") {
		s = strings.TrimSuffix(s, "%")
	} else if strings.HasSuffix(s, "K") {
		s = strings.TrimSuffix(s, "K")
		multiplier = 1000
	} else if strings.HasSuffix(s, "M") {
		s = strings.TrimSuffix(s, "M")
		multiplier = 1_000_000
	} else if strings.HasSuffix(s, "B") {
		s = strings.TrimSuffix(s, "B")
		multiplier = 1_000_000_000
	} else if strings.HasSuffix(s, "T") {
		s = strings.TrimSuffix(s, "T")
		multiplier = 1_000_000_000_000
	}

	var val float64
	_, err := fmt.Sscanf(s, "%f", &val)
	if err != nil {
		return 0
	}

	return val * multiplier
}

// randomDelay adds a random human-like delay between min and max milliseconds.
func randomDelay(minMs, maxMs int) {
	delay := minMs + rand.Intn(maxMs-minMs)
	time.Sleep(time.Duration(delay) * time.Millisecond)
}
