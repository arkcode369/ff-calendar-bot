// Package scheduler orchestrates all background periodic jobs.
// Each job runs on its own ticker, respects context cancellation,
// and logs errors without crashing the process.
//
// Jobs:
//   - FF Calendar scrape + alert scheduling
//   - FF Revision detection
//   - COT data fetch + analysis
//   - Surprise index recalculation
//   - Confluence score computation
//   - Volatility forecast
//   - Currency ranking
//   - Weekly outlook (Sunday 18:00 WIB)
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	aisvc "github.com/arkcode369/ff-calendar-bot/internal/service/ai"
	calsvc "github.com/arkcode369/ff-calendar-bot/internal/service/calendar"
	cotsvc "github.com/arkcode369/ff-calendar-bot/internal/service/cot"
	quantsvc "github.com/arkcode369/ff-calendar-bot/internal/service/quant"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// ---------------------------------------------------------------------------
// Dependencies & Configuration
// ---------------------------------------------------------------------------

// Deps holds all service dependencies the scheduler needs.
type Deps struct {
	CalService          *calsvc.Service
	CalAlerter          *calsvc.Alerter
	COTAnalyzer         *cotsvc.Analyzer
	SurpriseCalc        *quantsvc.SurpriseCalculator
	ConfluenceScorer    *quantsvc.ConfluenceScorer
	VolatilityPredictor *quantsvc.VolatilityPredictor
	CurrencyRanker      *quantsvc.CurrencyRanker
	AIAnalyzer          *aisvc.Interpreter
	Bot                 ports.Messenger
	EventRepo           ports.EventRepository
	COTRepo             ports.COTRepository
	SurpriseRepo        ports.SurpriseRepository
	ChatID              string
}

// Intervals configures how often each job runs.
type Intervals struct {
	FFScrape       time.Duration // Default: 30m
	FFRevision     time.Duration // Default: 2h
	COTFetch       time.Duration // Default: 6h
	SurpriseCalc   time.Duration // Default: 1h
	ConfluenceCalc time.Duration // Default: 2h
}

// ---------------------------------------------------------------------------
// Scheduler
// ---------------------------------------------------------------------------

// Scheduler manages all background periodic jobs.
type Scheduler struct {
	deps    *Deps
	stopCh  chan struct{}
	wg      sync.WaitGroup
	running bool
	mu      sync.Mutex
}

// New creates a new Scheduler.
func New(deps *Deps) *Scheduler {
	return &Scheduler{
		deps:   deps,
		stopCh: make(chan struct{}),
	}
}

// Start launches all background jobs. Non-blocking.
func (s *Scheduler) Start(ctx context.Context, intervals *Intervals) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		log.Println("[SCHED] Already running")
		return
	}
	s.running = true

	// FF Calendar scrape
	s.startJob(ctx, "ff-scrape", intervals.FFScrape, s.jobFFScrape)

	// FF Revision detection (offset by half the scrape interval)
	s.startJobWithDelay(ctx, "ff-revision", intervals.FFRevision, intervals.FFScrape/2, s.jobFFRevision)

	// COT fetch + analysis
	s.startJob(ctx, "cot-fetch", intervals.COTFetch, s.jobCOTFetch)

	// Surprise index recalculation
	s.startJobWithDelay(ctx, "surprise-calc", intervals.SurpriseCalc, 5*time.Minute, s.jobSurpriseCalc)

	// Confluence score computation
	s.startJobWithDelay(ctx, "confluence-calc", intervals.ConfluenceCalc, 10*time.Minute, s.jobConfluenceCalc)

	// Volatility forecast (every 4 hours)
	s.startJobWithDelay(ctx, "volatility", 4*time.Hour, 15*time.Minute, s.jobVolatility)

	// Currency ranking (every 2 hours)
	s.startJobWithDelay(ctx, "currency-rank", 2*time.Hour, 20*time.Minute, s.jobCurrencyRank)

	// Weekly outlook (check every hour, fires on Sunday 18:00 WIB)
	s.startJob(ctx, "weekly-outlook", 1*time.Hour, s.jobWeeklyOutlook)

	log.Printf("[SCHED] Started 8 background jobs")
}

// Stop signals all jobs to stop and waits for them to finish.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	log.Println("[SCHED] Stopping all jobs...")
	close(s.stopCh)
	s.wg.Wait()
	s.running = false
	log.Println("[SCHED] All jobs stopped")
}

// ---------------------------------------------------------------------------
// Job Launcher Helpers
// ---------------------------------------------------------------------------

type jobFunc func(ctx context.Context) error

// startJob launches a goroutine that runs fn on every tick.
func (s *Scheduler) startJob(ctx context.Context, name string, interval time.Duration, fn jobFunc) {
	s.startJobWithDelay(ctx, name, interval, 0, fn)
}

// startJobWithDelay launches a goroutine with an initial delay before first tick.
func (s *Scheduler) startJobWithDelay(ctx context.Context, name string, interval, delay time.Duration, fn jobFunc) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		// Initial delay to stagger jobs
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		log.Printf("[SCHED] Job %q started (interval=%v, delay=%v)", name, interval, delay)

		for {
			select {
			case <-ticker.C:
				s.runJob(ctx, name, fn)
			case <-ctx.Done():
				log.Printf("[SCHED] Job %q: context cancelled", name)
				return
			case <-s.stopCh:
				log.Printf("[SCHED] Job %q: stop signal received", name)
				return
			}
		}
	}()
}

// runJob executes a job with timeout, panic recovery, and logging.
func (s *Scheduler) runJob(ctx context.Context, name string, fn jobFunc) {
	start := time.Now()

	// Per-job timeout: 5 minutes max
	jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[SCHED] PANIC in job %q: %v", name, r)
		}
	}()

	if err := fn(jobCtx); err != nil {
		log.Printf("[SCHED] Job %q failed (took %v): %v", name, time.Since(start), err)
	} else {
		log.Printf("[SCHED] Job %q completed (took %v)", name, time.Since(start))
	}
}

// ---------------------------------------------------------------------------
// Job Implementations
// ---------------------------------------------------------------------------

// jobFFScrape scrapes FF calendar, stores events, and schedules alerts.
func (s *Scheduler) jobFFScrape(ctx context.Context) error {
	newCount, updCount, err := s.deps.CalService.ScrapeAndStore(ctx)
	if err != nil {
		return fmt.Errorf("ff scrape: %w", err)
	}

	log.Printf("[SCHED:ff-scrape] %d new, %d updated events", newCount, updCount)

	// Schedule alerts for upcoming events
	now := timeutil.NowWIB()
	end := now.Add(24 * time.Hour)
	events, err := s.deps.EventRepo.GetHighImpactEvents(ctx, now, end)
	if err != nil {
		log.Printf("[SCHED:ff-scrape] Failed to get upcoming events for alerts: %v", err)
	} else {
		s.deps.CalAlerter.ScheduleAlerts(ctx, events)
	}

	// Enrich with historical data (background)
	allEvents, _ := s.deps.EventRepo.GetEventsByDateRange(ctx, now, end)
	if len(allEvents) > 0 {
		go func() {
			histCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := s.deps.CalService.FetchAndStoreHistory(histCtx, allEvents); err != nil {
				log.Printf("[SCHED:ff-scrape] History enrichment failed: %v", err)
			}
		}()
	}

	return nil
}

// jobFFRevision checks for data revisions in recently released events.
func (s *Scheduler) jobFFRevision(ctx context.Context) error {
	now := timeutil.NowWIB()
	start := now.Add(-48 * time.Hour) // Check last 48 hours

	events, err := s.deps.EventRepo.GetEventsByDateRange(ctx, start, now)
	if err != nil {
		return fmt.Errorf("get events for revision check: %w", err)
	}

	// Filter to only released events
	var released []domain.FFEvent
	for _, ev := range events {
		if ev.Actual != "" {
			released = append(released, ev)
		}
	}

	if len(released) == 0 {
		return nil
	}

	_, _, err = s.deps.CalService.ScrapeAndStore(ctx)
	if err != nil {
		return fmt.Errorf("revision scrape: %w", err)
	}
	return nil
}

// jobCOTFetch fetches latest COT data from CFTC and runs analysis.
func (s *Scheduler) jobCOTFetch(ctx context.Context) error {
	if _, err := s.deps.COTAnalyzer.AnalyzeAll(ctx); err != nil {
		return fmt.Errorf("cot fetch+analyze: %w", err)
	}

	log.Println("[SCHED:cot-fetch] COT data fetched and analyzed")
	return nil
}

// jobSurpriseCalc recalculates surprise indices for all currencies.
func (s *Scheduler) jobSurpriseCalc(ctx context.Context) error {
	if _, err := s.deps.SurpriseCalc.ComputeAll(ctx); err != nil {
		return fmt.Errorf("surprise recalc: %w", err)
	}

	log.Println("[SCHED:surprise-calc] Surprise indices recalculated")
	return nil
}

// jobConfluenceCalc computes confluence scores for major pairs.
func (s *Scheduler) jobConfluenceCalc(ctx context.Context) error {
	pairs := []struct{ base, quote string }{
		{"EUR", "USD"}, {"GBP", "USD"}, {"USD", "JPY"}, {"AUD", "USD"},
		{"NZD", "USD"}, {"USD", "CAD"}, {"USD", "CHF"}, {"EUR", "GBP"},
		{"EUR", "JPY"}, {"GBP", "JPY"}, {"AUD", "NZD"}, {"EUR", "AUD"},
	}

	var errs []error
	for _, p := range pairs {
		score, err := s.deps.ConfluenceScorer.ComputeForPair(ctx, p.base, p.quote)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s%s: %w", p.base, p.quote, err))
			continue
		}
		if err := s.deps.SurpriseRepo.SaveConfluence(ctx, *score); err != nil {
			errs = append(errs, fmt.Errorf("save %s%s: %w", p.base, p.quote, err))
		}
	}

	log.Printf("[SCHED:confluence-calc] Computed %d pairs, %d errors", len(pairs), len(errs))

	if len(errs) > 0 {
		return fmt.Errorf("confluence errors: %v", errs[0]) // Return first error
	}
	return nil
}

// jobVolatility computes volatility forecasts.
func (s *Scheduler) jobVolatility(ctx context.Context) error {
	forecast, err := s.deps.VolatilityPredictor.PredictUpcoming(ctx, 48)
	if err != nil {
		return fmt.Errorf("volatility predict: %w", err)
	}

	if err := s.deps.SurpriseRepo.SaveVolatilityForecast(ctx, *forecast); err != nil {
		return fmt.Errorf("save volatility forecast: %w", err)
	}

	log.Printf("[SCHED:volatility] Forecast: risk_window=%s, max_expected=%.1f",
		forecast.RiskWindow, forecast.MaxExpected)
	return nil
}

// jobCurrencyRank computes currency strength rankings.
func (s *Scheduler) jobCurrencyRank(ctx context.Context) error {
	ranking, err := s.deps.CurrencyRanker.RankAll(ctx)
	if err != nil {
		return fmt.Errorf("currency rank: %w", err)
	}

	if err := s.deps.SurpriseRepo.SaveCurrencyRanking(ctx, *ranking); err != nil {
		return fmt.Errorf("save ranking: %w", err)
	}

	log.Printf("[SCHED:currency-rank] Ranked %d currencies", len(ranking.Rankings))
	return nil
}

// jobWeeklyOutlook generates and sends the weekly outlook on Sunday evening.
// Fires every hour but only executes on Sunday between 18:00-18:59 WIB.
func (s *Scheduler) jobWeeklyOutlook(ctx context.Context) error {
	now := timeutil.NowWIB()

	// Only fire on Sunday 18:xx WIB
	if now.Weekday() != time.Sunday || now.Hour() != 18 {
		return nil
	}

	// Check if AI is available
	if s.deps.AIAnalyzer == nil || !s.deps.AIAnalyzer.IsAvailable() {
		log.Println("[SCHED:weekly-outlook] AI not available, skipping")
		return nil
	}

	// Gather all data for the outlook
	data, err := s.gatherWeeklyData(ctx)
	if err != nil {
		return fmt.Errorf("gather weekly data: %w", err)
	}

	// Generate outlook via AI
	outlook, err := s.deps.AIAnalyzer.GenerateWeeklyOutlook(ctx, data)
	if err != nil {
		return fmt.Errorf("generate outlook: %w", err)
	}

	// Send to chat
	msg := fmt.Sprintf("<b>\xF0\x9F\x93\x8B Weekly Macro Outlook</b>\n\n%s", outlook)
	if _, err := s.deps.Bot.SendHTML(ctx, s.deps.ChatID, msg); err != nil {
		return fmt.Errorf("send outlook: %w", err)
	}

	log.Println("[SCHED:weekly-outlook] Weekly outlook sent")
	return nil
}

// gatherWeeklyData collects all data needed for the weekly outlook.
func (s *Scheduler) gatherWeeklyData(ctx context.Context) (ports.WeeklyData, error) {
	var data ports.WeeklyData

	// COT analyses
	analyses, err := s.deps.COTRepo.GetAllLatestAnalyses(ctx)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] COT analyses unavailable: %v", err)
	} else {
		data.COTAnalyses = analyses
	}

	// Surprise indices
	indices, err := s.deps.SurpriseRepo.GetAllSurpriseIndices(ctx)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Surprise indices unavailable: %v", err)
	} else {
		data.SurpriseIndices = indices
	}

	// Confluence scores
	confluences, err := s.deps.SurpriseRepo.GetAllConfluences(ctx)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Confluence scores unavailable: %v", err)
	} else {
		data.ConfluenceScores = confluences
	}

	// Currency ranking
	ranking, err := s.deps.SurpriseRepo.GetLatestRanking(ctx)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Currency ranking unavailable: %v", err)
	} else {
		data.CurrencyRanking = ranking
	}

	// Upcoming week events
	now := timeutil.NowWIB()
	weekEnd := now.Add(7 * 24 * time.Hour)
	events, err := s.deps.EventRepo.GetHighImpactEvents(ctx, now, weekEnd)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Upcoming events unavailable: %v", err)
	} else {
		data.UpcomingEvents = events
	}

	// Volatility forecast
	vol, err := s.deps.SurpriseRepo.GetLatestVolatilityForecast(ctx)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Volatility forecast unavailable: %v", err)
	} else {
		data.VolatilityForecast = vol
	}

	// Recent revisions
	revisions, err := s.deps.EventRepo.GetAllRevisions(ctx, 7)
	if err != nil {
		log.Printf("[SCHED:weekly-outlook] Revisions unavailable: %v", err)
	} else {
		data.RecentRevisions = revisions
	}

	return data, nil
}
