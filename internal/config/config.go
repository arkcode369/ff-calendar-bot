// Package config manages application configuration via environment variables.
// All settings have sensible defaults. Required vars fail fast on startup.
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	// Required
	BotToken string // Telegram bot token
	ChatID   string // Default Telegram chat ID

	// AI (optional — graceful degradation without)
	GeminiAPIKey string // Google Gemini API key
	GeminiModel  string // Model name (default: gemini-2.0-flash)

	// Storage
	DataDir string // BadgerDB data directory

	// Scraping
	FFScrapeInterval      time.Duration // How often to scrape FF calendar
	FFHistoricalInterval  time.Duration // How often to scrape historical data
	FFRevisionInterval    time.Duration // How often to check for revisions
	FFBaseURL             string        // ForexFactory base URL
	FFJSONFallbackURL     string        // Fair Economy JSON mirror

	// COT
	COTFetchInterval time.Duration // How often to fetch COT data
	COTSocrataURL    string        // CFTC Socrata API URL
	COTHistoryWeeks  int           // How many weeks of COT history to maintain

	// Quantitative
	SurpriseCalcInterval    time.Duration // Surprise index recalculation interval
	ConfluenceCalcInterval  time.Duration // Confluence score recalculation interval
	SurpriseDecayHalfLife   float64       // Half-life in days for surprise decay
	SurpriseWindowDays      int           // Lookback window for surprise index

	// AI
	AICacheTTL    time.Duration // How long to cache AI responses
	AIMaxRPM      int           // Max requests per minute to Gemini

	// Logging
	LogLevel string // "debug", "info", "warn", "error"

	// Alerts
	DefaultAlertMinutes []int    // Default alert minutes before event
	DefaultAlertImpacts []string // Default impact levels to alert
}

// MustLoad loads configuration from environment variables.
// Panics if required variables are missing.
func MustLoad() *Config {
	cfg := &Config{
		// Required (will panic if empty)
		BotToken: mustGetEnv("BOT_TOKEN"),
		ChatID:   mustGetEnv("CHAT_ID"),

		// AI (optional)
		GeminiAPIKey: getEnv("GEMINI_API_KEY", ""),
		GeminiModel:  getEnv("GEMINI_MODEL", "gemini-2.0-flash"),

		// Storage
		DataDir: getEnv("DATA_DIR", "/app/data"),

		// Scraping
		FFScrapeInterval:     getDuration("FF_SCRAPE_INTERVAL", 30*time.Minute),
		FFHistoricalInterval: getDuration("FF_HISTORICAL_INTERVAL", 168*time.Hour), // weekly
		FFRevisionInterval:   getDuration("FF_REVISION_INTERVAL", 2*time.Hour),
		FFBaseURL:            getEnv("FF_BASE_URL", "https://www.forexfactory.com"),
		FFJSONFallbackURL:    getEnv("FF_JSON_FALLBACK_URL", "https://nfs.faireconomy.media/ff_calendar_thisweek.json"),

		// COT
		COTFetchInterval: getDuration("COT_FETCH_INTERVAL", 6*time.Hour),
		COTSocrataURL:    getEnv("COT_SOCRATA_URL", "https://publicreporting.cftc.gov/resource/6dca-aqww.json"),
		COTHistoryWeeks:  getInt("COT_HISTORY_WEEKS", 52),

		// Quantitative
		SurpriseCalcInterval:   getDuration("SURPRISE_CALC_INTERVAL", 1*time.Hour),
		ConfluenceCalcInterval: getDuration("CONFLUENCE_CALC_INTERVAL", 2*time.Hour),
		SurpriseDecayHalfLife:  getFloat("SURPRISE_DECAY_HALFLIFE", 30.0),
		SurpriseWindowDays:     getInt("SURPRISE_WINDOW_DAYS", 90),

		// AI
		AICacheTTL: getDuration("AI_CACHE_TTL", 1*time.Hour),
		AIMaxRPM:   getInt("AI_MAX_RPM", 15),

		// Logging
		LogLevel: getEnv("LOG_LEVEL", "info"),

		// Alerts
		DefaultAlertMinutes: getIntSlice("DEFAULT_ALERT_MINUTES", []int{60, 15, 5, 1}),
		DefaultAlertImpacts: getStringSlice("DEFAULT_ALERT_IMPACTS", []string{"High", "Medium"}),
	}

	cfg.validate()
	return cfg
}

// HasGemini returns true if Gemini API key is configured.
func (c *Config) HasGemini() bool {
	return c.GeminiAPIKey != ""
}

// validate performs additional validation beyond required env vars.
func (c *Config) validate() {
	if c.FFScrapeInterval < 1*time.Minute {
		log.Fatal("[CONFIG] FF_SCRAPE_INTERVAL must be >= 1 minute")
	}
	if c.COTHistoryWeeks < 4 {
		log.Fatal("[CONFIG] COT_HISTORY_WEEKS must be >= 4")
	}
	if c.SurpriseDecayHalfLife <= 0 {
		log.Fatal("[CONFIG] SURPRISE_DECAY_HALFLIFE must be > 0")
	}
}

// ---------------------------------------------------------------------------
// Env Helpers
// ---------------------------------------------------------------------------

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("[CONFIG] Required env var %s is not set", key)
	}
	return v
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Printf("[CONFIG] Invalid duration for %s=%q, using default %v", key, v, defaultVal)
			return defaultVal
		}
		return d
	}
	return defaultVal
}

func getInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("[CONFIG] Invalid int for %s=%q, using default %d", key, v, defaultVal)
			return defaultVal
		}
		return n
	}
	return defaultVal
}

func getFloat(key string, defaultVal float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Printf("[CONFIG] Invalid float for %s=%q, using default %f", key, v, defaultVal)
			return defaultVal
		}
		return f
	}
	return defaultVal
}

func getIntSlice(key string, defaultVal []int) []int {
	if v := os.Getenv(key); v != "" {
		parts := strings.Split(v, ",")
		result := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil {
				continue
			}
			result = append(result, n)
		}
		if len(result) > 0 {
			return result
		}
	}
	return defaultVal
}

func getStringSlice(key string, defaultVal []string) []string {
	if v := os.Getenv(key); v != "" {
		parts := strings.Split(v, ",")
		result := make([]string, 0, len(parts))
		for _, p := range parts {
			s := strings.TrimSpace(p)
			if s != "" {
				result = append(result, s)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return defaultVal
}

// String returns a redacted configuration summary for logging.
func (c *Config) String() string {
	geminiStatus := "NOT CONFIGURED"
	if c.HasGemini() {
		geminiStatus = "CONFIGURED"
	}
	return fmt.Sprintf(
		"Config{DataDir=%s, FFInterval=%v, COTInterval=%v, Gemini=%s, LogLevel=%s}",
		c.DataDir, c.FFScrapeInterval, c.COTFetchInterval, geminiStatus, c.LogLevel,
	)
}
