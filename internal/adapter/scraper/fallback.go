package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// ---------------------------------------------------------------------------
// FallbackScraper — Fair Economy JSON API fallback
// ---------------------------------------------------------------------------

// FallbackScraper fetches calendar data from the Fair Economy JSON mirror.
// Used as a fallback when direct ForexFactory HTML scraping fails due to
// Cloudflare challenges, rate limiting, or site structure changes.
//
// API endpoint: https://nfs.faireconomy.media/ff_calendar_thisweek.json
// This public API mirrors ForexFactory data in a clean JSON format,
// updated every ~15 minutes.
type FallbackScraper struct {
	jsonURL    string
	httpClient *http.Client
}

// fairEconomyEvent represents a single event from the Fair Economy JSON API.
type fairEconomyEvent struct {
	Title    string `json:"title"`
	Country  string `json:"country"`
	Date     string `json:"date"`     // "2024-03-11T08:30:00-04:00"
	Impact   string `json:"impact"`   // "High", "Medium", "Low", "Holiday"
	Forecast string `json:"forecast"` // "200K", "3.5%", etc.
	Previous string `json:"previous"` // "150K", "3.4%", etc.
}

// NewFallbackScraper creates a Fair Economy JSON fallback scraper.
func NewFallbackScraper(jsonURL string) *FallbackScraper {
	if jsonURL == "" {
		jsonURL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"
	}
	return &FallbackScraper{
		jsonURL: jsonURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        5,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

// FetchWeeklyJSON fetches this week's calendar data from Fair Economy.
func (f *FallbackScraper) FetchWeeklyJSON(ctx context.Context) ([]domain.FFEvent, error) {
	log.Printf("[FALLBACK] Fetching from Fair Economy: %s", f.jsonURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.jsonURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Set browser-like headers
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch fair economy: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fair economy returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024)) // 5MB limit
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var raw []fairEconomyEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	events := make([]domain.FFEvent, 0, len(raw))
	for _, r := range raw {
		event, err := f.convertEvent(r)
		if err != nil {
			log.Printf("[FALLBACK] Skip event %q: %v", r.Title, err)
			continue
		}
		events = append(events, event)
	}

	log.Printf("[FALLBACK] Fetched %d events from Fair Economy (%d raw)", len(events), len(raw))
	return events, nil
}

// convertEvent transforms a Fair Economy JSON event into a domain FFEvent.
func (f *FallbackScraper) convertEvent(raw fairEconomyEvent) (domain.FFEvent, error) {
	// Parse the ISO 8601 datetime (ET timezone)
	eventTime, err := time.Parse(time.RFC3339, raw.Date)
	if err != nil {
		// Try alternative formats
		eventTime, err = time.Parse("2006-01-02T15:04:05-07:00", raw.Date)
		if err != nil {
			return domain.FFEvent{}, fmt.Errorf("parse date %q: %w", raw.Date, err)
		}
	}

	// Convert to WIB
	wibTime := eventTime.In(timeutil.WIB)

	// Map country to currency code
	currency := countryToCurrency(raw.Country)

	// Parse impact
	impact := parseImpactString(raw.Impact)

	// Detect category from title
	category := detectCategoryFromTitle(raw.Title)

	// Determine if all-day event
	isAllDay := false
	if impact == domain.ImpactNone || raw.Impact == "Holiday" {
		isAllDay = true
	}

	// Build time string in ET for display
	et, _ := time.LoadLocation("America/New_York")
	etTime := eventTime.In(et)
	timeStr := etTime.Format("3:04pm")
	if isAllDay {
		timeStr = "All Day"
	}

	// Generate event ID
	eventID := fmt.Sprintf("%s:%s:%08x",
		wibTime.Format("2006-01-02"),
		currency,
		fnvHash(raw.Title),
	)

	event := domain.FFEvent{
		ID:          eventID,
		Title:       raw.Title,
		Currency:    currency,
		Country:     raw.Country,
		Date:        wibTime,
		Time:        timeStr,
		IsAllDay:    isAllDay,
		Impact:      impact,
		Category:    category,
		Forecast:    raw.Forecast,
		Previous:    raw.Previous,
		ReleaseType: detectReleaseTypeFromTitle(raw.Title),
		ScrapedAt:   timeutil.NowWIB(),
		Source:      "faireconomy",
	}

	// Detect preliminary/final
	event.IsPreliminary = event.ReleaseType == domain.ReleasePreliminary
	event.IsFinal = event.ReleaseType == domain.ReleaseFinal

	// Detect speaker for speech events
	if category == domain.CategorySpeech {
		event.SpeakerName, event.SpeakerRole = detectSpeakerFromTitle(raw.Title)
	}

	return event, nil
}

// ---------------------------------------------------------------------------
// Mapping helpers
// ---------------------------------------------------------------------------

// countryToCurrency maps Fair Economy country names to 3-letter currency codes.
func countryToCurrency(country string) string {
	mapping := map[string]string{
		"USD": "USD", "United States": "USD", "US": "USD",
		"EUR": "EUR", "European Monetary Union": "EUR", "EU": "EUR",
		"GBP": "GBP", "United Kingdom": "GBP", "UK": "GBP",
		"JPY": "JPY", "Japan": "JPY",
		"AUD": "AUD", "Australia": "AUD",
		"NZD": "NZD", "New Zealand": "NZD",
		"CAD": "CAD", "Canada": "CAD",
		"CHF": "CHF", "Switzerland": "CHF",
		"CNY": "CNY", "China": "CNY",
	}

	if code, ok := mapping[country]; ok {
		return code
	}

	// If already a 3-letter code, return as-is
	if len(country) == 3 {
		return strings.ToUpper(country)
	}

	return "OTHER"
}

// parseImpactString converts Fair Economy impact strings to domain ImpactLevel.
func parseImpactString(impact string) domain.ImpactLevel {
	switch strings.ToLower(strings.TrimSpace(impact)) {
	case "high":
		return domain.ImpactHigh
	case "medium":
		return domain.ImpactMedium
	case "low":
		return domain.ImpactLow
	default:
		return domain.ImpactNone
	}
}

// detectCategoryFromTitle classifies an event based on its title.
func detectCategoryFromTitle(title string) domain.EventCategory {
	lower := strings.ToLower(title)

	cbKeywords := []string{"rate decision", "monetary policy", "rate statement",
		"meeting minutes", "policy report", "mpc ", "fomc "}
	for _, kw := range cbKeywords {
		if strings.Contains(lower, kw) {
			return domain.CategoryCentralBank
		}
	}

	speechKeywords := []string{"speaks", "speech", "testimony", "testifies",
		"press conference", "remarks"}
	for _, kw := range speechKeywords {
		if strings.Contains(lower, kw) {
			return domain.CategorySpeech
		}
	}

	if strings.Contains(lower, "auction") || strings.Contains(lower, "bond") {
		return domain.CategoryAuction
	}

	if strings.Contains(lower, "holiday") || strings.Contains(lower, "day off") {
		return domain.CategoryHoliday
	}

	return domain.CategoryEconomicIndicator
}

// detectReleaseTypeFromTitle identifies preliminary/revised/final releases.
func detectReleaseTypeFromTitle(title string) domain.ReleaseType {
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

// detectSpeakerFromTitle extracts speaker name and role.
func detectSpeakerFromTitle(title string) (string, string) {
	speakers := map[string]string{
		"Powell": "Fed Chair", "Lagarde": "ECB President",
		"Bailey": "BOE Governor", "Ueda": "BOJ Governor",
		"Macklem": "BOC Governor", "Bullock": "RBA Governor",
		"Orr": "RBNZ Governor", "Jordan": "SNB Chairman",
		"Waller": "Fed Governor", "Williams": "NY Fed President",
		"Bowman": "Fed Governor", "Barr": "Fed Vice Chair",
		"Schnabel": "ECB Board", "Lane": "ECB Chief Economist",
		"Pill": "BOE Chief Economist",
	}

	for name, role := range speakers {
		if strings.Contains(title, name) {
			return name, role
		}
	}

	// Generic: "[Name] Speaks"
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

// fnvHash computes a simple FNV-1a hash for string-to-ID generation.
func fnvHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
