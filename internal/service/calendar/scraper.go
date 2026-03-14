package calendar

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

// Service orchestrates ForexFactory calendar operations:
// scraping, parsing, revision detection, history collection, and alert scheduling.
type Service struct {
	scraper   ports.FFScraper
	eventRepo ports.EventRepository
	messenger ports.Messenger
	alerter   *Alerter
}

// NewService creates a calendar service with all dependencies.
func NewService(scraper ports.FFScraper, repo ports.EventRepository, messenger ports.Messenger) *Service {
	s := &Service{
		scraper:   scraper,
		eventRepo: repo,
		messenger: messenger,
	}
	s.alerter = NewAlerter(repo, messenger)
	return s
}

// ScrapeAndStore fetches the current week's calendar, detects revisions,
// stores events, and schedules alerts. Returns new/updated event count.
func (s *Service) ScrapeAndStore(ctx context.Context) (newCount, updatedCount int, err error) {
	// 1. Scrape current week
	events, err := s.scraper.ScrapeWeeklyCalendar(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("scrape weekly calendar: %w", err)
	}
	if len(events) == 0 {
		log.Println("[calendar] no events scraped")
		return 0, 0, nil
	}

	log.Printf("[calendar] scraped %d events", len(events))

	// 2. Detect revisions by comparing with stored events
	now := timeutil.NowWIB()
	start := timeutil.StartOfDayWIB(now)
	end := start.AddDate(0, 0, 7)

	existing, err := s.eventRepo.GetEventsByDateRange(ctx, start, end)
	if err != nil {
		log.Printf("[calendar] warn: could not load existing events: %v", err)
		existing = nil
	}

	existingMap := buildEventMap(existing)

	var revisions []domain.EventRevision
	for i := range events {
		ev := &events[i]
		key := eventKey(ev)
		if old, ok := existingMap[key]; ok {
			if revs := detectRevisions(old, ev, now); len(revs) > 0 {
				revisions = append(revisions, revs...)
				updatedCount++
			}
		} else {
			newCount++
		}
	}

	// 3. Save revisions
	for _, rev := range revisions {
		if err := s.eventRepo.SaveRevision(ctx, rev); err != nil {
			log.Printf("[calendar] warn: save revision: %v", err)
		}
	}

	// 4. Store all events (upsert)
	if err := s.eventRepo.SaveEvents(ctx, events); err != nil {
		return newCount, updatedCount, fmt.Errorf("save events: %w", err)
	}

	// 5. Schedule alerts for upcoming high-impact events
	s.alerter.ScheduleAlerts(ctx, events)

	log.Printf("[calendar] stored %d events (%d new, %d updated, %d revisions)",
		len(events), newCount, updatedCount, len(revisions))

	return newCount, updatedCount, nil
}

// GetTodayEvents returns all events for today in WIB timezone.
func (s *Service) GetTodayEvents(ctx context.Context) ([]domain.FFEvent, error) {
	now := timeutil.NowWIB()
	start := timeutil.StartOfDayWIB(now)
	end := start.AddDate(0, 0, 1)
	return s.eventRepo.GetEventsByDateRange(ctx, start, end)
}

// GetWeekEvents returns all events for the current week.
func (s *Service) GetWeekEvents(ctx context.Context) ([]domain.FFEvent, error) {
	now := timeutil.NowWIB()
	start := timeutil.StartOfWeekWIB(now)
	end := start.AddDate(0, 0, 7)
	return s.eventRepo.GetEventsByDateRange(ctx, start, end)
}

// GetUpcomingHighImpact returns high-impact events within the next N hours.
func (s *Service) GetUpcomingHighImpact(ctx context.Context, hours int) ([]domain.FFEvent, error) {
	now := timeutil.NowWIB()
	end := now.Add(time.Duration(hours) * time.Hour)

	events, err := s.eventRepo.GetEventsByDateRange(ctx, now, end)
	if err != nil {
		return nil, err
	}

	var highImpact []domain.FFEvent
	for _, ev := range events {
		if ev.Impact == domain.ImpactHigh {
			highImpact = append(highImpact, ev)
		}
	}
	return highImpact, nil
}

// GetEventWithHistory returns an event's historical data for analysis.
func (s *Service) GetEventWithHistory(ctx context.Context, eventName, currency string, months int) ([]domain.FFEventDetail, error) {
	return s.eventRepo.GetEventHistory(ctx, eventName, currency, months)
}

// FetchAndStoreHistory scrapes historical data for high-impact events
// that don't have history yet. Runs as background enrichment.
func (s *Service) FetchAndStoreHistory(ctx context.Context, events []domain.FFEvent) error {
	for _, ev := range events {
		if ev.Impact != domain.ImpactHigh || ev.SourceURL == "" {
			continue
		}

		// Check if we already have history
		existing, err := s.eventRepo.GetEventHistory(ctx, ev.Title, ev.Currency, 1)
		if err == nil && len(existing) > 0 {
			continue
		}

		history, err := s.scraper.ScrapeEventHistory(ctx, ev.SourceURL)
		if err != nil {
			log.Printf("[calendar] warn: scrape history for %s: %v", ev.Title, err)
			continue
		}

		if err := s.eventRepo.SaveEventDetails(ctx, history); err != nil {
			log.Printf("[calendar] warn: save history for %s: %v", ev.Title, err)
		}

		// Rate limit: don't hammer FF
		time.Sleep(2 * time.Second)
	}
	return nil
}

// GetRecentRevisions returns data revisions for a currency in the last N days.
func (s *Service) GetRecentRevisions(ctx context.Context, currency string, days int) ([]domain.EventRevision, error) {
	return s.eventRepo.GetRevisions(ctx, currency, days)
}

// --- helpers ---

func buildEventMap(events []domain.FFEvent) map[string]*domain.FFEvent {
	m := make(map[string]*domain.FFEvent, len(events))
	for i := range events {
		m[eventKey(&events[i])] = &events[i]
	}
	return m
}

func eventKey(ev *domain.FFEvent) string {
	return fmt.Sprintf("%s|%s|%s", ev.Date.Format(time.RFC3339), ev.Currency, ev.Title)
}

// detectRevisions compares old vs new event data for revisions.
func detectRevisions(old, new *domain.FFEvent, now time.Time) []domain.EventRevision {
	var revs []domain.EventRevision

	// Check Actual revision
	if old.Actual != "" && new.Actual != "" && old.Actual != new.Actual {
		revs = append(revs, domain.EventRevision{
			EventName:     new.Title,
			Currency:      new.Currency,
			Field:         "actual",
			OriginalValue: old.Actual,
			RevisedValue:  new.Actual,
			RevisionDate:  now,
		})
	}

	// Check Previous revision (common for GDP, NFP, etc.)
	if old.Previous != "" && new.Previous != "" && old.Previous != new.Previous {
		revs = append(revs, domain.EventRevision{
			EventName:     new.Title,
			Currency:      new.Currency,
			Field:         "previous",
			OriginalValue: old.Previous,
			RevisedValue:  new.Previous,
			RevisionDate:  now,
		})
	}

	// Check Forecast revision
	if old.Forecast != "" && new.Forecast != "" && old.Forecast != new.Forecast {
		revs = append(revs, domain.EventRevision{
			EventName:     new.Title,
			Currency:      new.Currency,
			Field:         "forecast",
			OriginalValue: old.Forecast,
			RevisedValue:  new.Forecast,
			RevisionDate:  now,
		})
	}

	// Detect preliminary -> final transition
	if old.IsPreliminary && !new.IsPreliminary && new.Actual != "" {
		revs = append(revs, domain.EventRevision{
			EventName:     new.Title,
			Currency:      new.Currency,
			Field:         "status",
			OriginalValue: "preliminary",
			RevisedValue:  "final",
			RevisionDate:  now,
		})
	}

	// Set direction on each revision
	for i := range revs {
		revs[i].Direction = classifyRevisionDirection(revs[i].OriginalValue, revs[i].RevisedValue)
	}

	return revs
}

// classifyRevisionDirection determines if a revision is upward/downward.
func classifyRevisionDirection(original, revised string) domain.RevisionDirection {
	o := strings.TrimRight(strings.TrimRight(original, "%"), "KkMmBb")
	r := strings.TrimRight(strings.TrimRight(revised, "%"), "KkMmBb")

	ov := parseFloat(o)
	rv := parseFloat(r)

	if rv > ov {
		return domain.RevisionUp
	} else if rv < ov {
		return domain.RevisionDown
	}
	return domain.RevisionFlat
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}
