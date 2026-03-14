package telegram

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// ---------------------------------------------------------------------------
// Handler — Wires services to Telegram commands
// ---------------------------------------------------------------------------

// Handler holds all service dependencies and registers commands on the bot.
type Handler struct {
	bot        *Bot
	fmt        *Formatter
	kb         *KeyboardBuilder

	// Repositories
	eventRepo    ports.EventRepository
	cotRepo      ports.COTRepository
	surpriseRepo ports.SurpriseRepository
	prefsRepo    ports.PrefsRepository

	// Services (called via ports interfaces where possible)
	scraper    ports.FFScraper
	aiAnalyzer ports.AIAnalyzer
}

// NewHandler creates a handler and registers all commands on the bot.
func NewHandler(
	bot *Bot,
	eventRepo ports.EventRepository,
	cotRepo ports.COTRepository,
	surpriseRepo ports.SurpriseRepository,
	prefsRepo ports.PrefsRepository,
	scraper ports.FFScraper,
	aiAnalyzer ports.AIAnalyzer,
) *Handler {
	h := &Handler{
		bot:          bot,
		fmt:          NewFormatter(),
		kb:           NewKeyboardBuilder(),
		eventRepo:    eventRepo,
		cotRepo:      cotRepo,
		surpriseRepo: surpriseRepo,
		prefsRepo:    prefsRepo,
		scraper:      scraper,
		aiAnalyzer:   aiAnalyzer,
	}

	// Register all commands
	bot.RegisterCommand("/start", h.cmdStart)
	bot.RegisterCommand("/help", h.cmdHelp)
	bot.RegisterCommand("/today", h.cmdToday)
	bot.RegisterCommand("/week", h.cmdWeek)
	bot.RegisterCommand("/nextweek", h.cmdNextWeek)
	bot.RegisterCommand("/cot", h.cmdCOT)
	bot.RegisterCommand("/rank", h.cmdRank)
	bot.RegisterCommand("/confluence", h.cmdConfluence)
	bot.RegisterCommand("/surprise", h.cmdSurprise)
	bot.RegisterCommand("/volatility", h.cmdVolatility)
	bot.RegisterCommand("/outlook", h.cmdOutlook)
	bot.RegisterCommand("/settings", h.cmdSettings)
	bot.RegisterCommand("/status", h.cmdStatus)
	bot.RegisterCommand("/refresh", h.cmdRefresh)

	// Register callback handlers
	bot.RegisterCallback("cot:", h.cbCOTDetail)
	bot.RegisterCallback("conf:", h.cbConfluenceDetail)
	bot.RegisterCallback("alert:", h.cbAlertToggle)
	bot.RegisterCallback("set:", h.cbSettings)

	log.Printf("[HANDLER] Registered 14 commands and 4 callback prefixes")
	return h
}

// ---------------------------------------------------------------------------
// /start & /help — Onboarding
// ---------------------------------------------------------------------------

func (h *Handler) cmdStart(ctx context.Context, chatID string, userID int64, args string) error {
	html := `<b>FF Calendar Bot v2</b>

Institutional-grade forex fundamental analysis:

<b>Calendar Commands:</b>
/today - Today's economic events
/week - This week's calendar
/nextweek - Next week's calendar

<b>Analysis Commands:</b>
/cot - COT positioning analysis
/rank - Currency strength ranking
/confluence - Multi-factor confluence scores
/surprise - Economic surprise indices
/volatility - News volatility forecast
/outlook - AI weekly market outlook

<b>System Commands:</b>
/settings - Alert preferences
/status - Bot health status
/refresh - Force data refresh
/help - Show this menu

Alerts are <b>enabled</b> by default for High+Medium impact events at 60, 15, 5, and 1 minute before release.`

	_, err := h.bot.SendHTML(ctx, chatID, html)
	return err
}

func (h *Handler) cmdHelp(ctx context.Context, chatID string, userID int64, args string) error {
	return h.cmdStart(ctx, chatID, userID, args)
}

// ---------------------------------------------------------------------------
// /today — Today's economic events
// ---------------------------------------------------------------------------

func (h *Handler) cmdToday(ctx context.Context, chatID string, userID int64, args string) error {
	now := timeutil.NowWIB()
	start := timeutil.StartOfDay(now)
	end := timeutil.EndOfDay(now)

	var events []domain.FFEvent
	var err error

	// Filter by currency if argument provided
	if args != "" {
		currency := strings.ToUpper(strings.TrimSpace(args))
		events, err = h.eventRepo.GetEventsByCurrency(ctx, currency, start, end)
	} else {
		events, err = h.eventRepo.GetEventsByDate(ctx, now)
	}

	if err != nil {
		return fmt.Errorf("fetch today events: %w", err)
	}

	if len(events) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID,
			fmt.Sprintf("No economic events scheduled for today (%s).", now.Format("Mon, Jan 2")))
		return err
	}

	html := h.fmt.FormatDailyCalendar(events, now)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /week — This week's calendar
// ---------------------------------------------------------------------------

func (h *Handler) cmdWeek(ctx context.Context, chatID string, userID int64, args string) error {
	now := timeutil.NowWIB()
	start := timeutil.StartOfWeek(now)
	end := start.AddDate(0, 0, 7)

	var events []domain.FFEvent
	var err error

	if args != "" {
		// Filter: /week high or /week USD
		arg := strings.ToUpper(strings.TrimSpace(args))
		if arg == "HIGH" {
			events, err = h.eventRepo.GetHighImpactEvents(ctx, start, end)
		} else {
			events, err = h.eventRepo.GetEventsByCurrency(ctx, arg, start, end)
		}
	} else {
		events, err = h.eventRepo.GetEventsByDateRange(ctx, start, end)
	}

	if err != nil {
		return fmt.Errorf("fetch week events: %w", err)
	}

	if len(events) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID, "No events found for this week.")
		return err
	}

	html := h.fmt.FormatWeeklyCalendar(events, start)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /nextweek — Next week's calendar
// ---------------------------------------------------------------------------

func (h *Handler) cmdNextWeek(ctx context.Context, chatID string, userID int64, args string) error {
	now := timeutil.NowWIB()
	// Next week start is 7 days after this week start
	start := timeutil.StartOfWeek(now).AddDate(0, 0, 7)
	end := start.AddDate(0, 0, 7)

	var events []domain.FFEvent
	var err error

	if args != "" {
		arg := strings.ToUpper(strings.TrimSpace(args))
		if arg == "HIGH" {
			events, err = h.eventRepo.GetHighImpactEvents(ctx, start, end)
		} else {
			events, err = h.eventRepo.GetEventsByCurrency(ctx, arg, start, end)
		}
	} else {
		events, err = h.eventRepo.GetEventsByDateRange(ctx, start, end)
	}

	if err != nil {
		return fmt.Errorf("fetch next week events: %w", err)
	}

	if len(events) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID, "No events found for next week yet. Try /refresh to fetch latest data.")
		return err
	}

	html := h.fmt.FormatWeeklyCalendar(events, start)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /cot — COT positioning analysis
// ---------------------------------------------------------------------------

func (h *Handler) cmdCOT(ctx context.Context, chatID string, userID int64, args string) error {
	// If specific currency requested: /cot USD
	if args != "" {
		code := strings.ToUpper(strings.TrimSpace(args))
		contractCode := currencyToContractCode(code)
		analysis, err := h.cotRepo.GetLatestAnalysis(ctx, contractCode)
		if err != nil {
			return fmt.Errorf("get COT analysis for %s: %w", code, err)
		}
		if analysis == nil {
			_, err = h.bot.SendHTML(ctx, chatID,
				fmt.Sprintf("No COT analysis available for <b>%s</b>. Data may not have been fetched yet.", code))
			return err
		}

		html := h.fmt.FormatCOTDetail(*analysis)

		// Add AI interpretation if available
		if h.aiAnalyzer != nil && h.aiAnalyzer.IsAvailable() {
			narrative, aiErr := h.aiAnalyzer.AnalyzeCOT(ctx, []domain.COTAnalysis{*analysis})
			if aiErr == nil && narrative != "" {
				html += "\n\n" + h.fmt.FormatAIInsight("COT Analysis", narrative)
			}
		}

		_, err = h.bot.SendHTML(ctx, chatID, html)
		return err
	}

	// Overview: all currencies
	analyses, err := h.cotRepo.GetAllLatestAnalyses(ctx)
	if err != nil {
		return fmt.Errorf("get all COT analyses: %w", err)
	}

	if len(analyses) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID,
			"No COT data available yet. Data is fetched from CFTC every Friday.")
		return err
	}

	html := h.fmt.FormatCOTOverview(analyses)
	kb := h.kb.COTCurrencySelector(analyses)
	_, err = h.bot.SendWithKeyboard(ctx, chatID, html, kb)
	return err
}

// cbCOTDetail handles inline keyboard callback for COT detail view.
func (h *Handler) cbCOTDetail(ctx context.Context, chatID string, msgID int, userID int64, data string) error {
	// data format: "cot:USD" or "cot:6E"
	code := strings.TrimPrefix(data, "cot:")
	contractCode := currencyToContractCode(code)

	analysis, err := h.cotRepo.GetLatestAnalysis(ctx, contractCode)
	if err != nil || analysis == nil {
		return h.bot.EditMessage(ctx, chatID, msgID,
			fmt.Sprintf("No COT data for %s", code))
	}

	html := h.fmt.FormatCOTDetail(*analysis)
	return h.bot.EditMessage(ctx, chatID, msgID, html)
}

// ---------------------------------------------------------------------------
// /rank — Currency strength ranking
// ---------------------------------------------------------------------------

func (h *Handler) cmdRank(ctx context.Context, chatID string, userID int64, args string) error {
	ranking, err := h.surpriseRepo.GetLatestRanking(ctx)
	if err != nil {
		return fmt.Errorf("get currency ranking: %w", err)
	}

	if ranking == nil {
		_, err = h.bot.SendHTML(ctx, chatID,
			"No currency ranking available yet. Rankings are calculated periodically.")
		return err
	}

	html := h.fmt.FormatCurrencyRanking(*ranking)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /confluence — Multi-factor confluence scores
// ---------------------------------------------------------------------------

func (h *Handler) cmdConfluence(ctx context.Context, chatID string, userID int64, args string) error {
	if args != "" {
		// Specific pair: /confluence EURUSD
		pair := strings.ToUpper(strings.TrimSpace(args))
		score, err := h.surpriseRepo.GetLatestConfluence(ctx, pair)
		if err != nil {
			return fmt.Errorf("get confluence for %s: %w", pair, err)
		}
		if score == nil {
			_, err = h.bot.SendHTML(ctx, chatID,
				fmt.Sprintf("No confluence score for <b>%s</b>.", pair))
			return err
		}

		html := h.fmt.FormatConfluenceDetail(*score)

		// AI synthesis if available
		if h.aiAnalyzer != nil && h.aiAnalyzer.IsAvailable() {
			narrative, aiErr := h.aiAnalyzer.SynthesizeConfluence(ctx, *score)
			if aiErr == nil && narrative != "" {
				html += "\n\n" + h.fmt.FormatAIInsight("Confluence", narrative)
			}
		}

		_, err = h.bot.SendHTML(ctx, chatID, html)
		return err
	}

	// Overview: all pairs
	scores, err := h.surpriseRepo.GetAllConfluences(ctx)
	if err != nil {
		return fmt.Errorf("get all confluences: %w", err)
	}

	if len(scores) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID,
			"No confluence scores available yet. Scores are recalculated every 2 hours.")
		return err
	}

	html := h.fmt.FormatConfluenceOverview(scores)
	kb := h.kb.ConfluencePairSelector(scores)
	_, err = h.bot.SendWithKeyboard(ctx, chatID, html, kb)
	return err
}

// cbConfluenceDetail handles inline keyboard callback for confluence detail.
func (h *Handler) cbConfluenceDetail(ctx context.Context, chatID string, msgID int, userID int64, data string) error {
	pair := strings.TrimPrefix(data, "conf:")
	score, err := h.surpriseRepo.GetLatestConfluence(ctx, pair)
	if err != nil || score == nil {
		return h.bot.EditMessage(ctx, chatID, msgID,
			fmt.Sprintf("No confluence data for %s", pair))
	}

	html := h.fmt.FormatConfluenceDetail(*score)
	return h.bot.EditMessage(ctx, chatID, msgID, html)
}

// ---------------------------------------------------------------------------
// /surprise — Economic surprise indices
// ---------------------------------------------------------------------------

func (h *Handler) cmdSurprise(ctx context.Context, chatID string, userID int64, args string) error {
	indices, err := h.surpriseRepo.GetAllSurpriseIndices(ctx)
	if err != nil {
		return fmt.Errorf("get surprise indices: %w", err)
	}

	if len(indices) == 0 {
		_, err = h.bot.SendHTML(ctx, chatID,
			"No surprise index data available yet.")
		return err
	}

	html := h.fmt.FormatSurpriseIndices(indices)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /volatility — News volatility forecast
// ---------------------------------------------------------------------------

func (h *Handler) cmdVolatility(ctx context.Context, chatID string, userID int64, args string) error {
	forecast, err := h.surpriseRepo.GetLatestVolatilityForecast(ctx)
	if err != nil {
		return fmt.Errorf("get volatility forecast: %w", err)
	}

	if forecast == nil {
		_, err = h.bot.SendHTML(ctx, chatID,
			"No volatility forecast available yet.")
		return err
	}

	html := h.fmt.FormatVolatilityForecast(*forecast)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /outlook — AI weekly market outlook
// ---------------------------------------------------------------------------

func (h *Handler) cmdOutlook(ctx context.Context, chatID string, userID int64, args string) error {
	if h.aiAnalyzer == nil || !h.aiAnalyzer.IsAvailable() {
		_, err := h.bot.SendHTML(ctx, chatID,
			"AI outlook is unavailable. Gemini API key not configured.")
		return err
	}

	// Send "generating..." placeholder
	placeholderID, _ := h.bot.SendHTML(ctx, chatID, "Generating weekly outlook... (this may take 10-15s)")

	// Gather all data
	now := timeutil.NowWIB()
	start := timeutil.StartOfWeek(now)
	end := start.AddDate(0, 0, 7)

	cotAnalyses, _ := h.cotRepo.GetAllLatestAnalyses(ctx)
	surpriseIndices, _ := h.surpriseRepo.GetAllSurpriseIndices(ctx)
	confluenceScores, _ := h.surpriseRepo.GetAllConfluences(ctx)
	ranking, _ := h.surpriseRepo.GetLatestRanking(ctx)
	forecast, _ := h.surpriseRepo.GetLatestVolatilityForecast(ctx)
	upcomingEvents, _ := h.eventRepo.GetHighImpactEvents(ctx, start, end)
	revisions, _ := h.eventRepo.GetAllRevisions(ctx, 7)

	weeklyData := ports.WeeklyData{
		COTAnalyses:        cotAnalyses,
		SurpriseIndices:    surpriseIndices,
		ConfluenceScores:   confluenceScores,
		CurrencyRanking:    ranking,
		UpcomingEvents:     upcomingEvents,
		VolatilityForecast: forecast,
		RecentRevisions:    revisions,
	}

	outlook, err := h.aiAnalyzer.GenerateWeeklyOutlook(ctx, weeklyData)
	if err != nil {
		_ = h.bot.EditMessage(ctx, chatID, placeholderID,
			fmt.Sprintf("Failed to generate outlook: %v", err))
		return err
	}

	html := h.fmt.FormatWeeklyOutlook(outlook, now)

	// Delete placeholder and send full outlook
	_ = h.bot.DeleteMessage(ctx, chatID, placeholderID)
	_, err = h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /settings — User preferences
// ---------------------------------------------------------------------------

func (h *Handler) cmdSettings(ctx context.Context, chatID string, userID int64, args string) error {
	prefs, err := h.prefsRepo.Get(ctx, userID)
	if err != nil {
		return fmt.Errorf("get preferences: %w", err)
	}

	html := h.fmt.FormatSettings(prefs)
	kb := h.kb.SettingsMenu(prefs)
	_, err = h.bot.SendWithKeyboard(ctx, chatID, html, kb)
	return err
}

// cbSettings handles settings toggle callbacks.
func (h *Handler) cbSettings(ctx context.Context, chatID string, msgID int, userID int64, data string) error {
	action := strings.TrimPrefix(data, "set:")

	prefs, err := h.prefsRepo.Get(ctx, userID)
	if err != nil {
		return err
	}

	switch action {
	case "alerts_toggle":
		prefs.AlertsEnabled = !prefs.AlertsEnabled
	case "ai_toggle":
		prefs.AIReportsEnabled = !prefs.AIReportsEnabled
	case "impact_high_only":
		prefs.AlertImpacts = []string{"High"}
	case "impact_high_med":
		prefs.AlertImpacts = []string{"High", "Medium"}
	case "impact_all":
		prefs.AlertImpacts = []string{"High", "Medium", "Low"}
	case "time_60_15_5":
		prefs.AlertMinutes = []int{60, 15, 5}
	case "time_15_5":
		prefs.AlertMinutes = []int{15, 5}
	case "time_5_1":
		prefs.AlertMinutes = []int{5, 1}
	default:
		log.Printf("[HANDLER] Unknown settings action: %s", action)
		return nil
	}

	if err := h.prefsRepo.Set(ctx, userID, prefs); err != nil {
		return fmt.Errorf("save preferences: %w", err)
	}

	// Update the message with new settings state
	html := h.fmt.FormatSettings(prefs)
	kb := h.kb.SettingsMenu(prefs)
	return h.bot.EditWithKeyboard(ctx, chatID, msgID, html, kb)
}

// cbAlertToggle handles quick alert toggle from notification messages.
func (h *Handler) cbAlertToggle(ctx context.Context, chatID string, msgID int, userID int64, data string) error {
	action := strings.TrimPrefix(data, "alert:")

	prefs, err := h.prefsRepo.Get(ctx, userID)
	if err != nil {
		return err
	}

	switch action {
	case "mute_1h":
		// Temporarily disable alerts (would need a timer mechanism)
		prefs.AlertsEnabled = false
		_ = h.prefsRepo.Set(ctx, userID, prefs)
		return h.bot.EditMessage(ctx, chatID, msgID,
			"Alerts muted. Use /settings to re-enable.")
	case "dismiss":
		return h.bot.DeleteMessage(ctx, chatID, msgID)
	}

	return nil
}

// ---------------------------------------------------------------------------
// /status — Bot health and data freshness
// ---------------------------------------------------------------------------

func (h *Handler) cmdStatus(ctx context.Context, chatID string, userID int64, args string) error {
	now := timeutil.NowWIB()

	// Check scraper health
	scraperStatus := "OK"
	if err := h.scraper.HealthCheck(ctx); err != nil {
		scraperStatus = fmt.Sprintf("ERROR: %v", err)
	}

	// Check data freshness
	todayEvents, _ := h.eventRepo.GetEventsByDate(ctx, now)
	cotAnalyses, _ := h.cotRepo.GetAllLatestAnalyses(ctx)
	surpriseIndices, _ := h.surpriseRepo.GetAllSurpriseIndices(ctx)

	// AI status
	aiStatus := "Not configured"
	if h.aiAnalyzer != nil {
		if h.aiAnalyzer.IsAvailable() {
			aiStatus = "Available"
		} else {
			aiStatus = "Configured but unavailable"
		}
	}

	html := fmt.Sprintf(`<b>System Status</b>
<code>Time:       %s WIB</code>

<b>Data Sources:</b>
<code>Scraper:    %s</code>
<code>Events:     %d today</code>
<code>COT:        %d contracts</code>
<code>Surprise:   %d currencies</code>

<b>Services:</b>
<code>AI Engine:  %s</code>

<b>Version:</b> v2.0.0`,
		now.Format("15:04:05"),
		scraperStatus,
		len(todayEvents),
		len(cotAnalyses),
		len(surpriseIndices),
		aiStatus,
	)

	_, err := h.bot.SendHTML(ctx, chatID, html)
	return err
}

// ---------------------------------------------------------------------------
// /refresh — Force data refresh
// ---------------------------------------------------------------------------

func (h *Handler) cmdRefresh(ctx context.Context, chatID string, userID int64, args string) error {
	msgID, _ := h.bot.SendHTML(ctx, chatID, "Refreshing data...")

	start := time.Now()
	events, err := h.scraper.ScrapeWeeklyCalendar(ctx)
	if err != nil {
		_ = h.bot.EditMessage(ctx, chatID, msgID,
			fmt.Sprintf("Refresh failed: %v", err))
		return err
	}

	if err := h.eventRepo.SaveEvents(ctx, events); err != nil {
		_ = h.bot.EditMessage(ctx, chatID, msgID,
			fmt.Sprintf("Save failed: %v", err))
		return err
	}

	elapsed := time.Since(start)

	// Count by impact
	high, med, low := 0, 0, 0
	for _, e := range events {
		switch e.Impact {
		case domain.ImpactHigh:
			high++
		case domain.ImpactMedium:
			med++
		case domain.ImpactLow:
			low++
		}
	}

	html := fmt.Sprintf(`<b>Data Refreshed</b> (%.1fs)

<code>Total:  %d events</code>
<code>High:   %d</code>
<code>Medium: %d</code>
<code>Low:    %d</code>`,
		elapsed.Seconds(), len(events), high, med, low)

	_ = h.bot.EditMessage(ctx, chatID, msgID, html)
	return nil
}

// ---------------------------------------------------------------------------
// Currency-to-contract mapping
// ---------------------------------------------------------------------------

// currencyToContractCode maps 3-letter currency codes to CFTC contract codes.
func currencyToContractCode(currency string) string {
	mapping := map[string]string{
		"EUR": "099741", // Euro FX
		"GBP": "096742", // British Pound
		"JPY": "097741", // Japanese Yen
		"AUD": "232741", // Australian Dollar
		"NZD": "112741", // New Zealand Dollar
		"CAD": "090741", // Canadian Dollar
		"CHF": "092741", // Swiss Franc
		"USD": "098662", // US Dollar Index
		"GOLD": "088691", // Gold
		"XAU":  "088691", // Gold alias
		"OIL":  "067651", // Crude Oil
	}

	if code, ok := mapping[strings.ToUpper(currency)]; ok {
		return code
	}
	return currency // Return as-is if not mapped
}
