package calendar

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/domain"
	"github.com/arkcode369/ff-calendar-bot/internal/ports"
	"github.com/arkcode369/ff-calendar-bot/pkg/timeutil"
)

// Alerter manages scheduled alerts for upcoming high-impact events.
// It runs timers that fire N minutes before each event, sending
// a Telegram notification with event details and historical context.
type Alerter struct {
	eventRepo ports.EventRepository
	messenger ports.Messenger

	mu       sync.Mutex
	timers   map[string]*time.Timer // eventKey -> timer
	chatIDs  []string               // chat IDs to notify
	defaults []int                  // default alert minutes: [60, 15, 5]
}

// NewAlerter creates an alerter with default alert windows.
func NewAlerter(repo ports.EventRepository, messenger ports.Messenger) *Alerter {
	return &Alerter{
		eventRepo: repo,
		messenger: messenger,
		timers:    make(map[string]*time.Timer),
		defaults:  []int{60, 15, 5}, // 1 hour, 15 min, 5 min before
	}
}

// SetChatIDs configures which Telegram chats receive alerts.
func (a *Alerter) SetChatIDs(ids []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chatIDs = ids
}

// SetAlertMinutes configures custom alert windows (minutes before event).
func (a *Alerter) SetAlertMinutes(minutes []int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sort.Sort(sort.Reverse(sort.IntSlice(minutes)))
	a.defaults = minutes
}

// ScheduleAlerts sets up timers for all upcoming high-impact events.
// Called after each scrape cycle to refresh alert schedules.
func (a *Alerter) ScheduleAlerts(ctx context.Context, events []domain.FFEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := timeutil.NowWIB()
	scheduled := 0

	for _, ev := range events {
		// Only alert for high-impact events
		if ev.Impact != domain.ImpactHigh {
			continue
		}

		// Skip past events
		if ev.DateTime.Before(now) {
			continue
		}

		// Skip all-day events (no specific time)
		if ev.IsAllDay {
			continue
		}

		for _, mins := range a.defaults {
			key := fmt.Sprintf("%s|%s|%s|%d", ev.DateTime.Format(time.RFC3339), ev.Currency, ev.Title, mins)

			// Skip if already scheduled
			if _, exists := a.timers[key]; exists {
				continue
			}

			alertTime := ev.DateTime.Add(-time.Duration(mins) * time.Minute)
			delay := alertTime.Sub(now)

			// Skip if alert time already passed
			if delay <= 0 {
				continue
			}

			// Capture loop variables for closure
			event := ev
			minsBefore := mins

			timer := time.AfterFunc(delay, func() {
				a.fireAlert(ctx, event, minsBefore)
				a.mu.Lock()
				delete(a.timers, key)
				a.mu.Unlock()
			})

			a.timers[key] = timer
			scheduled++
		}
	}

	if scheduled > 0 {
		log.Printf("[alerter] scheduled %d alerts for %d events", scheduled, len(events))
	}
}

// CancelAll cancels all pending alert timers.
func (a *Alerter) CancelAll() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for key, timer := range a.timers {
		timer.Stop()
		delete(a.timers, key)
	}
	log.Println("[alerter] cancelled all pending alerts")
}

// PendingCount returns the number of pending alerts.
func (a *Alerter) PendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.timers)
}

// fireAlert sends an alert message for an upcoming event.
func (a *Alerter) fireAlert(ctx context.Context, ev domain.FFEvent, minsBefore int) {
	a.mu.Lock()
	chats := make([]string, len(a.chatIDs))
	copy(chats, a.chatIDs)
	a.mu.Unlock()

	if len(chats) == 0 {
		log.Printf("[alerter] no chat IDs configured, skipping alert for %s", ev.Title)
		return
	}

	msg := formatAlertMessage(ev, minsBefore)

	for _, chatID := range chats {
		if err := a.messenger.SendMessage(ctx, chatID, msg); err != nil {
			log.Printf("[alerter] send to %s: %v", chatID, err)
		}
	}
}

// formatAlertMessage creates a Telegram-formatted alert message.
func formatAlertMessage(ev domain.FFEvent, minsBefore int) string {
	var b strings.Builder

	// Header with urgency indicator
	switch {
	case minsBefore <= 5:
		b.WriteString(">>> IMMINENT <<<\n")
	case minsBefore <= 15:
		b.WriteString(">> ALERT <<\n")
	default:
		b.WriteString("> UPCOMING <\n")
	}

	b.WriteString(fmt.Sprintf("[HIGH IMPACT] %s %s\n", ev.Currency, ev.Title))
	b.WriteString(fmt.Sprintf("Time: %s (%d min away)\n",
		ev.DateTime.Format("15:04 WIB"), minsBefore))

	if ev.Forecast != "" {
		b.WriteString(fmt.Sprintf("Forecast: %s", ev.Forecast))
		if ev.Previous != "" {
			b.WriteString(fmt.Sprintf(" | Previous: %s", ev.Previous))
		}
		b.WriteString("\n")
	} else if ev.Previous != "" {
		b.WriteString(fmt.Sprintf("Previous: %s\n", ev.Previous))
	}

	if ev.SpeakerName != "" {
		b.WriteString(fmt.Sprintf("Speaker: %s\n", ev.SpeakerName))
	}

	if ev.IsPreliminary {
		b.WriteString("Note: Preliminary/Flash release\n")
	}

	return b.String()
}
