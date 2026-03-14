// Package main is the entry point for the FF Calendar Bot.
// It wires all dependencies using manual DI (no framework), starts
// background schedulers, and runs the Telegram long-polling loop.
//
// Shutdown is graceful: SIGINT/SIGTERM stops polling, drains in-flight
// handlers (10s deadline), cancels background jobs, flushes storage,
// then exits.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/adapter/scraper"
	"github.com/arkcode369/ff-calendar-bot/internal/adapter/storage"
	tgbot "github.com/arkcode369/ff-calendar-bot/internal/adapter/telegram"
	"github.com/arkcode369/ff-calendar-bot/internal/config"
	"github.com/arkcode369/ff-calendar-bot/internal/scheduler"
	aisvc "github.com/arkcode369/ff-calendar-bot/internal/service/ai"
	calsvc "github.com/arkcode369/ff-calendar-bot/internal/service/calendar"
	cotsvc "github.com/arkcode369/ff-calendar-bot/internal/service/cot"
	quantsvc "github.com/arkcode369/ff-calendar-bot/internal/service/quant"
)

const banner = `
╔══════════════════════════════════════════════════╗
║     FF CALENDAR BOT v2.0                         ║
║     Forex Factory • COT • Quant Analysis         ║
║     Built for institutional-grade macro intel     ║
╚══════════════════════════════════════════════════╝`

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	fmt.Println(banner)
	log.Printf("[MAIN] Starting FF Calendar Bot v2.0 (Go %s, %s/%s)",
		runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// -----------------------------------------------------------------------
	// 1. Configuration
	// -----------------------------------------------------------------------
	cfg := config.MustLoad()
	log.Printf("[MAIN] Config: %s", cfg)

	// -----------------------------------------------------------------------
	// 2. Root context with cancellation (drives graceful shutdown)
	// -----------------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// -----------------------------------------------------------------------
	// 3. Storage layer
	// -----------------------------------------------------------------------
	db, err := storage.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("[MAIN] Failed to open storage: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			log.Printf("[MAIN] Storage close error: %v", err)
		}
	}()

	eventRepo := storage.NewEventRepo(db)
	cotRepo := storage.NewCOTRepo(db)
	surpriseRepo := storage.NewSurpriseRepo(db)
	prefsRepo := storage.NewPrefsRepo(db)

	log.Println("[MAIN] Storage layer initialized")
	logStorageSize(db)

	// -----------------------------------------------------------------------
	// 4. Scraper layer (Colly + Fair Economy fallback)
	// -----------------------------------------------------------------------
	fallbackScraper := scraper.NewFallbackScraper(cfg.FFJSONFallbackURL)
	ffScraper := scraper.NewCollyFFScraper(cfg.FFBaseURL, fallbackScraper)

	log.Printf("[MAIN] Scraper initialized (base=%s)", cfg.FFBaseURL)

	// -----------------------------------------------------------------------
	// 5. Telegram bot
	// -----------------------------------------------------------------------
	bot := tgbot.NewBot(cfg.BotToken, cfg.ChatID)

	log.Println("[MAIN] Telegram bot created")

	// -----------------------------------------------------------------------
	// 6. AI layer (optional — graceful degradation)
	// -----------------------------------------------------------------------
	var aiAnalyzer *aisvc.Interpreter

	if cfg.HasGemini() {
		gemini, err := aisvc.NewGeminiClient(ctx, cfg.GeminiAPIKey)
		if err != nil {
			log.Printf("[MAIN] WARNING: Gemini init failed, AI features disabled: %v", err)
		} else {
			aiAnalyzer = aisvc.NewInterpreter(gemini, eventRepo, cotRepo)
			log.Println("[MAIN] Gemini AI initialized")
		}
	} else {
		log.Println("[MAIN] No GEMINI_API_KEY — AI features disabled (template fallback active)")
	}

	// -----------------------------------------------------------------------
	// 7. Service layer
	// -----------------------------------------------------------------------

	// Calendar services
	calService := calsvc.NewService(ffScraper, eventRepo, bot)
	calParser := calsvc.NewParser()
	calAlerter := calsvc.NewAlerter(eventRepo, bot)
	calAlerter.SetChatIDs([]string{cfg.ChatID})
	calAlerter.SetAlertMinutes(cfg.DefaultAlertMinutes)

	// COT services
	cotFetcher := cotsvc.NewFetcher()
	cotAnalyzer := cotsvc.NewAnalyzer(cotRepo, cotFetcher)
	cotIndexCalc := cotsvc.NewIndexCalculator()
	cotSignals := cotsvc.NewSignalDetector()

	// Quant services
	surpriseCalc := quantsvc.NewSurpriseCalculator(eventRepo, surpriseRepo)
	confluenceScorer := quantsvc.NewConfluenceScorer(eventRepo, cotRepo, surpriseRepo)
	volatilityPredictor := quantsvc.NewVolatilityPredictor(eventRepo)
	currencyRanker := quantsvc.NewCurrencyRanker(eventRepo, cotRepo, surpriseRepo)

	log.Println("[MAIN] Service layer initialized")

	// Suppress unused warnings for services used only by scheduler
	_ = calParser
	_ = cotIndexCalc
	_ = cotSignals

	// -----------------------------------------------------------------------
	// 8. Telegram handler (registers commands on bot)
	// -----------------------------------------------------------------------
	_ = tgbot.NewHandler(
		bot,
		eventRepo,
		cotRepo,
		surpriseRepo,
		prefsRepo,
		ffScraper,
		aiAnalyzer, // nil-safe: handler checks IsAvailable()
	)

	log.Println("[MAIN] Telegram handler registered")

	// -----------------------------------------------------------------------
	// 9. Background scheduler
	// -----------------------------------------------------------------------
	sched := scheduler.New(&scheduler.Deps{
		CalService:          calService,
		CalAlerter:          calAlerter,
		COTAnalyzer:         cotAnalyzer,
		SurpriseCalc:        surpriseCalc,
		ConfluenceScorer:    confluenceScorer,
		VolatilityPredictor: volatilityPredictor,
		CurrencyRanker:      currencyRanker,
		AIAnalyzer:          aiAnalyzer,
		Bot:                 bot,
		EventRepo:           eventRepo,
		COTRepo:             cotRepo,
		SurpriseRepo:        surpriseRepo,
		ChatID:              cfg.ChatID,
	})

	sched.Start(ctx, &scheduler.Intervals{
		FFScrape:       cfg.FFScrapeInterval,
		FFRevision:     cfg.FFRevisionInterval,
		COTFetch:       cfg.COTFetchInterval,
		SurpriseCalc:   cfg.SurpriseCalcInterval,
		ConfluenceCalc: cfg.ConfluenceCalcInterval,
	})

	log.Println("[MAIN] Background scheduler started")

	// -----------------------------------------------------------------------
	// 10. Initial data load (non-blocking)
	// -----------------------------------------------------------------------
	go func() {
		initCtx, initCancel := context.WithTimeout(ctx, 5*time.Minute)
		defer initCancel()

		log.Println("[MAIN] Running initial data load...")

		// Scrape calendar first
		newCount, updCount, err := calService.ScrapeAndStore(initCtx)
		if err != nil {
			log.Printf("[MAIN] Initial calendar scrape failed: %v", err)
		} else {
			log.Printf("[MAIN] Initial scrape: %d new, %d updated events", newCount, updCount)
		}

		// Fetch COT data
		if _, err := cotAnalyzer.AnalyzeAll(initCtx); err != nil {
			log.Printf("[MAIN] Initial COT fetch failed: %v", err)
		} else {
			log.Println("[MAIN] Initial COT data loaded")
		}

		// Compute surprise indices
		if _, err := surpriseCalc.ComputeAll(initCtx); err != nil {
			log.Printf("[MAIN] Initial surprise calc failed: %v", err)
		} else {
			log.Println("[MAIN] Initial surprise indices computed")
		}

		// Compute currency ranking
		if _, err := currencyRanker.RankAll(initCtx); err != nil {
			log.Printf("[MAIN] Initial ranking failed: %v", err)
		} else {
			log.Println("[MAIN] Initial currency ranking computed")
		}

		log.Println("[MAIN] Initial data load complete")

		// Send startup notification
		startupMsg := fmt.Sprintf(
			"<b>\xF0\x9F\x9F\xA2 FF Calendar Bot v2.0 Online</b>\n\n"+
				"Events: %d new, %d updated\n"+
				"AI: %s\n"+
				"Type /help for commands",
			newCount, updCount,
			aiStatus(aiAnalyzer),
		)
		if _, err := bot.SendHTML(initCtx, cfg.ChatID, startupMsg); err != nil {
			log.Printf("[MAIN] Failed to send startup notification: %v", err)
		}
	}()

	// -----------------------------------------------------------------------
	// 11. Signal handling & graceful shutdown
	// -----------------------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start polling in a goroutine
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		log.Println("[MAIN] Starting Telegram long-polling...")
		bot.StartPolling(ctx)
		log.Println("[MAIN] Polling stopped")
	}()

	// Block until signal
	sig := <-sigCh
	log.Printf("[MAIN] Received signal: %v — initiating graceful shutdown", sig)

	// Phase 1: Cancel context (stops polling + schedulers)
	cancel()

	// Phase 2: Wait for polling to drain (max 10s)
	select {
	case <-pollDone:
		log.Println("[MAIN] Polling drained cleanly")
	case <-time.After(10 * time.Second):
		log.Println("[MAIN] WARNING: Polling drain timed out after 10s")
	}

	// Phase 3: Stop scheduler
	sched.Stop()
	log.Println("[MAIN] Scheduler stopped")

	// Phase 4: Close storage (handled by defer)
	log.Println("[MAIN] Shutdown complete. Goodbye.")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// aiStatus returns a human-readable AI status string.
func aiStatus(ai *aisvc.Interpreter) string {
	if ai != nil && ai.IsAvailable() {
		return "Gemini active"
	}
	return "Template fallback"
}

// logStorageSize logs the current database size.
func logStorageSize(db *storage.DB) {
	lsm, vlog := db.Size()
	total := lsm + vlog
	if total > 1<<20 {
		log.Printf("[MAIN] Storage size: %.1f MB (LSM=%.1f MB, VLog=%.1f MB)",
			float64(total)/(1<<20), float64(lsm)/(1<<20), float64(vlog)/(1<<20))
	} else {
		log.Printf("[MAIN] Storage size: %d KB (LSM=%d KB, VLog=%d KB)",
			total>>10, lsm>>10, vlog>>10)
	}
}
