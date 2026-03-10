package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// CONFIG
// ============================================================================

var (
	BOT_TOKEN = os.Getenv("BOT_TOKEN")
	CHAT_ID   = os.Getenv("CHAT_ID") // target chat/group ID

	// Alert minutes before event
	ALERT_BEFORE = []int{30, 15, 5}

	// Timezone
	WIB *time.Location

	// Forex Factory JSON endpoint
	FF_JSON_URL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"
)

// ============================================================================
// DATA STRUCTURES
// ============================================================================

type FFEvent struct {
	Title    string `json:"title"`
	Country  string `json:"country"`
	Date     string `json:"date"`     // "2026-03-10T13:30:00-04:00"
	Impact   string `json:"impact"`   // "High", "Medium", "Low", "Holiday"
	Forecast string `json:"forecast"` // "3.1%"
	Previous string `json:"previous"` // "3.0%"
	Actual   string `json:"actual"`   // ""
}

type EventState struct {
	AlertedMinutes map[int]bool // which minute-alerts already sent
	ActualSent     bool         // already sent actual result
}

type Bot struct {
	token      string
	chatID     string
	client     *http.Client
	events     []FFEvent
	eventState map[string]*EventState // key: "title|date"
	mu         sync.RWMutex
	lastFetch  time.Time
	offset     int
}

// ============================================================================
// TELEGRAM API (raw HTTP, no bloated libs)
// ============================================================================

type TGUpdate struct {
	UpdateID int       `json:"update_id"`
	Message  *TGMessage `json:"message"`
}

type TGMessage struct {
	MessageID int    `json:"message_id"`
	Chat      TGChat `json:"chat"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

type TGChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type TGResponse struct {
	Ok     bool       `json:"ok"`
	Result []TGUpdate `json:"result"`
}

type TGSendResponse struct {
	Ok          bool   `json:"ok"`
	Description string `json:"description"`
}

func NewBot(token, chatID string) *Bot {
	return &Bot{
		token:      token,
		chatID:     chatID,
		client:     &http.Client{Timeout: 15 * time.Second},
		eventState: make(map[string]*EventState),
	}
}

func (b *Bot) apiURL(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.token, method)
}

func (b *Bot) sendMessage(chatID string, text string) error {
	if chatID == "" {
		chatID = b.chatID
	}
	url := b.apiURL("sendMessage")

	body := fmt.Sprintf(`{"chat_id":%s,"text":%s,"parse_mode":"HTML","disable_web_page_preview":true}`,
		jsonStr(chatID), jsonStr(text))

	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result TGSendResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Ok {
		return fmt.Errorf("telegram error: %s", result.Description)
	}
	return nil
}

func (b *Bot) getUpdates() ([]TGUpdate, error) {
	url := fmt.Sprintf("%s?offset=%d&timeout=1&allowed_updates=[\"message\"]", b.apiURL("getUpdates"), b.offset)

	resp, err := b.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result TGResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Ok {
		return nil, fmt.Errorf("getUpdates failed")
	}
	return result.Result, nil
}

// ============================================================================
// FOREX FACTORY DATA FETCHER
// ============================================================================

func (b *Bot) fetchEvents() error {
	req, err := http.NewRequest("GET", FF_JSON_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var events []FFEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return fmt.Errorf("json parse error: %w (status: %d)", err, resp.StatusCode)
	}

	b.mu.Lock()
	b.events = events
	b.lastFetch = time.Now()

	// init state for new events
	for _, e := range events {
		key := eventKey(e)
		if _, ok := b.eventState[key]; !ok {
			b.eventState[key] = &EventState{
				AlertedMinutes: make(map[int]bool),
			}
		}
	}
	b.mu.Unlock()

	log.Printf("[FETCH] Loaded %d events", len(events))
	return nil
}

func eventKey(e FFEvent) string {
	return e.Title + "|" + e.Date
}

func parseEventTime(dateStr string) (time.Time, error) {
	// FF format: "2026-03-10T13:30:00-04:00"
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		// fallback: "2026-03-10T13:30:00-0400"
		t, err = time.Parse("2006-01-02T15:04:05-0700", dateStr)
	}
	return t, err
}

// ============================================================================
// EVENT FORMATTING
// ============================================================================

func impactEmoji(impact string) string {
	switch strings.ToLower(impact) {
	case "high":
		return "!!!"  // RED ALERT
	case "medium":
		return "!! "  // ORANGE
	case "low":
		return "!  "  // YELLOW
	case "holiday":
		return "---"
	default:
		return "   "
	}
}

func impactTag(impact string) string {
	switch strings.ToLower(impact) {
	case "high":
		return "[HIGH]"
	case "medium":
		return "[MED]"
	case "low":
		return "[LOW]"
	case "holiday":
		return "[HOLIDAY]"
	default:
		return ""
	}
}

func formatEventLine(e FFEvent) string {
	t, err := parseEventTime(e.Date)
	if err != nil {
		return ""
	}
	wibTime := t.In(WIB)
	timeStr := wibTime.Format("15:04")

	result := fmt.Sprintf("%s  %s  %s  %s",
		impactEmoji(e.Impact), timeStr, e.Country, e.Title)

	// data line
	var dataparts []string
	if e.Forecast != "" {
		dataparts = append(dataparts, "F: "+e.Forecast)
	}
	if e.Previous != "" {
		dataparts = append(dataparts, "P: "+e.Previous)
	}
	if e.Actual != "" {
		dataparts = append(dataparts, "<b>A: "+e.Actual+"</b>")
	}
	if len(dataparts) > 0 {
		result += "\n         " + strings.Join(dataparts, "  |  ")
	}

	return result
}

func formatDayCalendar(events []FFEvent, day time.Time) string {
	dayStr := day.In(WIB).Format("2006-01-02")

	var dayEvents []FFEvent
	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.In(WIB).Format("2006-01-02") == dayStr {
			dayEvents = append(dayEvents, e)
		}
	}

	if len(dayEvents) == 0 {
		return fmt.Sprintf("<b>== %s ==</b>\nNo events scheduled.",
			day.In(WIB).Format("Mon, 02 Jan 2006"))
	}

	// sort by time
	sort.Slice(dayEvents, func(i, j int) bool {
		return dayEvents[i].Date < dayEvents[j].Date
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>== ECONOMIC CALENDAR ==</b>\n"))
	sb.WriteString(fmt.Sprintf("<b>%s (WIB)</b>\n",
		day.In(WIB).Format("Mon, 02 Jan 2006")))
	sb.WriteString("================================\n")

	// count high impact
	highCount := 0
	for _, e := range dayEvents {
		if strings.EqualFold(e.Impact, "high") {
			highCount++
		}
	}
	sb.WriteString(fmt.Sprintf("Total: %d events | HIGH IMPACT: %d\n", len(dayEvents), highCount))
	sb.WriteString("================================\n\n")

	for _, e := range dayEvents {
		line := formatEventLine(e)
		if line != "" {
			sb.WriteString(line + "\n\n")
		}
	}

	sb.WriteString("\n<i>!!! = HIGH  |  !! = MED  |  ! = LOW</i>\n")
	sb.WriteString("<i>F = Forecast  |  P = Previous  |  A = Actual</i>\n")
	sb.WriteString(fmt.Sprintf("<i>Source: ForexFactory | Updated: %s WIB</i>",
		time.Now().In(WIB).Format("15:04")))

	return sb.String()
}

func formatWeekCalendar(events []FFEvent) string {
	// group by day
	dayMap := make(map[string][]FFEvent)
	var days []string

	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		dayStr := t.In(WIB).Format("2006-01-02")
		if _, ok := dayMap[dayStr]; !ok {
			days = append(days, dayStr)
		}
		dayMap[dayStr] = append(dayMap[dayStr], e)
	}

	sort.Strings(days)

	var sb strings.Builder
	sb.WriteString("<b>== WEEKLY ECONOMIC CALENDAR (WIB) ==</b>\n\n")

	for _, dayStr := range days {
		devents := dayMap[dayStr]

		// sort by time
		sort.Slice(devents, func(i, j int) bool {
			return devents[i].Date < devents[j].Date
		})

		dt, _ := time.Parse("2006-01-02", dayStr)
		sb.WriteString(fmt.Sprintf("<b>--- %s ---</b>\n", dt.Format("Mon, 02 Jan")))

		for _, e := range devents {
			line := formatEventLine(e)
			if line != "" {
				sb.WriteString(line + "\n")
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString("<i>Source: ForexFactory</i>")
	return sb.String()
}

func formatHighImpact(events []FFEvent) string {
	var highEvents []FFEvent
	for _, e := range events {
		if strings.EqualFold(e.Impact, "high") {
			highEvents = append(highEvents, e)
		}
	}

	if len(highEvents) == 0 {
		return "No high-impact events this week."
	}

	sort.Slice(highEvents, func(i, j int) bool {
		return highEvents[i].Date < highEvents[j].Date
	})

	var sb strings.Builder
	sb.WriteString("<b>== HIGH IMPACT EVENTS (WIB) ==</b>\n\n")

	currentDay := ""
	for _, e := range highEvents {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		dayStr := t.In(WIB).Format("Mon, 02 Jan")
		if dayStr != currentDay {
			sb.WriteString(fmt.Sprintf("\n<b>%s</b>\n", dayStr))
			currentDay = dayStr
		}

		wibTime := t.In(WIB).Format("15:04")
		sb.WriteString(fmt.Sprintf("  %s  %s  %s\n", wibTime, e.Country, e.Title))

		var dataparts []string
		if e.Forecast != "" {
			dataparts = append(dataparts, "F: "+e.Forecast)
		}
		if e.Previous != "" {
			dataparts = append(dataparts, "P: "+e.Previous)
		}
		if e.Actual != "" {
			dataparts = append(dataparts, "<b>A: "+e.Actual+"</b>")
		}
		if len(dataparts) > 0 {
			sb.WriteString("         " + strings.Join(dataparts, "  |  ") + "\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\n<i>Total: %d high-impact events</i>\n", len(highEvents)))
	sb.WriteString("<i>Source: ForexFactory</i>")
	return sb.String()
}

func formatNextEvents(events []FFEvent, count int) string {
	now := time.Now().In(WIB)

	var upcoming []FFEvent
	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.In(WIB).After(now) {
			upcoming = append(upcoming, e)
		}
	}

	sort.Slice(upcoming, func(i, j int) bool {
		return upcoming[i].Date < upcoming[j].Date
	})

	if len(upcoming) == 0 {
		return "No upcoming events remaining this week."
	}

	if count > len(upcoming) {
		count = len(upcoming)
	}
	upcoming = upcoming[:count]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>== NEXT %d EVENTS (WIB) ==</b>\n\n", count))

	for _, e := range upcoming {
		t, _ := parseEventTime(e.Date)
		wibTime := t.In(WIB)
		diff := time.Until(wibTime)
		minsLeft := int(diff.Minutes())

		timeLabel := ""
		if minsLeft < 60 {
			timeLabel = fmt.Sprintf(" (in %dm)", minsLeft)
		} else {
			timeLabel = fmt.Sprintf(" (in %dh%dm)", minsLeft/60, minsLeft%60)
		}

		sb.WriteString(fmt.Sprintf("%s  %s  %s  %s%s\n",
			impactEmoji(e.Impact),
			wibTime.Format("15:04"),
			e.Country,
			e.Title,
			timeLabel))

		var dataparts []string
		if e.Forecast != "" {
			dataparts = append(dataparts, "F: "+e.Forecast)
		}
		if e.Previous != "" {
			dataparts = append(dataparts, "P: "+e.Previous)
		}
		if len(dataparts) > 0 {
			sb.WriteString("         " + strings.Join(dataparts, "  |  ") + "\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("<i>Source: ForexFactory</i>")
	return sb.String()
}

// ============================================================================
// ALERT ENGINE
// ============================================================================

func (b *Bot) checkAlerts() {
	b.mu.RLock()
	events := b.events
	b.mu.RUnlock()

	now := time.Now()

	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}

		key := eventKey(e)
		b.mu.RLock()
		state, ok := b.eventState[key]
		b.mu.RUnlock()
		if !ok {
			continue
		}

		diff := time.Until(t)
		minsLeft := int(diff.Minutes())

		// PRE-EVENT ALERTS
		for _, alertMin := range ALERT_BEFORE {
			if minsLeft <= alertMin && minsLeft > (alertMin-2) && !state.AlertedMinutes[alertMin] {
				// Send pre-event alert
				wibTime := t.In(WIB).Format("15:04")
				msg := fmt.Sprintf(
					"<b>== NEWS ALERT ==</b>\n"+
						"%s %s in <b>%d minutes</b>\n\n"+
						"%s  %s  %s\n"+
						"Time: %s WIB\n",
					impactTag(e.Impact), e.Country, minsLeft,
					impactEmoji(e.Impact), e.Country, e.Title,
					wibTime)

				if e.Forecast != "" {
					msg += fmt.Sprintf("Forecast: %s\n", e.Forecast)
				}
				if e.Previous != "" {
					msg += fmt.Sprintf("Previous: %s\n", e.Previous)
				}

				if err := b.sendMessage("", msg); err != nil {
					log.Printf("[ALERT] Failed to send pre-alert: %v", err)
				} else {
					log.Printf("[ALERT] Sent %dm alert: %s %s", alertMin, e.Country, e.Title)
				}

				b.mu.Lock()
				state.AlertedMinutes[alertMin] = true
				b.mu.Unlock()
			}
		}

		// POST-RELEASE: check if actual data appeared
		if e.Actual != "" && !state.ActualSent && now.After(t) {
			wibTime := t.In(WIB).Format("15:04")

			// Determine beat/miss
			verdict := ""
			if e.Forecast != "" && e.Actual != "" {
				actVal := parseNumber(e.Actual)
				fcVal := parseNumber(e.Forecast)
				if actVal > fcVal {
					verdict = "BETTER than forecast"
				} else if actVal < fcVal {
					verdict = "WORSE than forecast"
				} else {
					verdict = "IN LINE with forecast"
				}
			}

			msg := fmt.Sprintf(
				"<b>== NEWS RESULT ==</b>\n"+
					"%s %s\n\n"+
					"%s  %s\n"+
					"Time: %s WIB\n\n"+
					"<b>Actual:   %s</b>\n"+
					"Forecast: %s\n"+
					"Previous: %s\n",
				impactTag(e.Impact), e.Country,
				e.Country, e.Title,
				wibTime,
				e.Actual, e.Forecast, e.Previous)

			if verdict != "" {
				msg += fmt.Sprintf("\n<b>%s</b>\n", verdict)
			}

			if err := b.sendMessage("", msg); err != nil {
				log.Printf("[RESULT] Failed to send result: %v", err)
			} else {
				log.Printf("[RESULT] Sent actual: %s %s = %s", e.Country, e.Title, e.Actual)
			}

			b.mu.Lock()
			state.ActualSent = true
			b.mu.Unlock()
		}
	}
}

// ============================================================================
// COMMAND HANDLER
// ============================================================================

func (b *Bot) handleCommand(chatID int64, text string) {
	cid := fmt.Sprintf("%d", chatID)

	b.mu.RLock()
	events := b.events
	b.mu.RUnlock()

	switch {
	case text == "/start" || text == "/help":
		msg := "<b>== FF ECONOMIC CALENDAR BOT ==</b>\n\n" +
			"Commands:\n" +
			"/calendar - Today's economic events\n" +
			"/week - Full week calendar\n" +
			"/high - High impact events only\n" +
			"/next - Next 10 upcoming events\n" +
			"/alerts - Alert status info\n" +
			"/refresh - Force data refresh\n" +
			"/chatid - Show this chat's ID\n\n" +
			"<b>Auto alerts:</b>\n" +
			"- 30min, 15min, 5min before events\n" +
			"- Actual results when released\n\n" +
			"<i>All times in WIB (UTC+7)</i>\n" +
			"<i>Data: ForexFactory</i>"
		b.sendMessage(cid, msg)

	case text == "/calendar" || text == "/today":
		now := time.Now().In(WIB)
		msg := formatDayCalendar(events, now)
		b.sendMessage(cid, msg)

	case text == "/week":
		msg := formatWeekCalendar(events)
		// Split if too long (TG limit 4096)
		if len(msg) > 4000 {
			chunks := splitMessage(msg, 4000)
			for _, chunk := range chunks {
				b.sendMessage(cid, chunk)
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			b.sendMessage(cid, msg)
		}

	case text == "/high":
		msg := formatHighImpact(events)
		b.sendMessage(cid, msg)

	case text == "/next":
		msg := formatNextEvents(events, 10)
		b.sendMessage(cid, msg)

	case text == "/alerts":
		now := time.Now()
		upcoming := 0
		for _, e := range events {
			t, err := parseEventTime(e.Date)
			if err != nil {
				continue
			}
			if t.After(now) {
				upcoming++
			}
		}
		b.mu.RLock()
		lf := b.lastFetch.In(WIB).Format("15:04:05")
		b.mu.RUnlock()

		msg := fmt.Sprintf(
			"<b>== ALERT STATUS ==</b>\n\n"+
				"Events loaded: %d\n"+
				"Upcoming: %d\n"+
				"Last refresh: %s WIB\n"+
				"Alert target: %s\n"+
				"Alert before: %v min\n"+
				"Auto-refresh: 2min near events, 5min idle",
			len(events), upcoming, lf, b.chatID, ALERT_BEFORE)
		b.sendMessage(cid, msg)

	case text == "/refresh":
		if err := b.fetchEvents(); err != nil {
			b.sendMessage(cid, fmt.Sprintf("Refresh failed: %v", err))
		} else {
			b.sendMessage(cid, fmt.Sprintf("Refreshed: %d events loaded.", len(b.events)))
		}

	case text == "/chatid":
		b.sendMessage(cid, fmt.Sprintf("Chat ID: <code>%d</code>", chatID))
	}
}

// ============================================================================
// MAIN LOOP
// ============================================================================

func (b *Bot) run() {
	log.Println("[BOT] Starting FF Calendar Bot...")

	// Initial fetch
	if err := b.fetchEvents(); err != nil {
		log.Printf("[BOT] Initial fetch error: %v", err)
	}

	// Background: smart refresh - 2min when events are near, 5min otherwise
	go func() {
		for {
			interval := 5 * time.Minute

			// Check if any event is within 45 min window (before or after)
			b.mu.RLock()
			for _, e := range b.events {
				t, err := parseEventTime(e.Date)
				if err != nil {
					continue
				}
				diff := time.Until(t)
				// Fast refresh when event is 45min away or just happened (actuals pending)
				if diff > -30*time.Minute && diff < 45*time.Minute {
					interval = 2 * time.Minute
					break
				}
			}
			b.mu.RUnlock()

			time.Sleep(interval)
			if err := b.fetchEvents(); err != nil {
				log.Printf("[FETCH] Error: %v", err)
			}
		}
	}()

	// Background: check alerts every 30 seconds
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			b.checkAlerts()
		}
	}()

	// Foreground: poll for commands
	log.Println("[BOT] Polling for messages...")
	for {
		updates, err := b.getUpdates()
		if err != nil {
			log.Printf("[POLL] Error: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}

		for _, u := range updates {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}

			if u.Message != nil && strings.HasPrefix(u.Message.Text, "/") {
				// strip @botname from commands in groups
				cmd := u.Message.Text
				if idx := strings.Index(cmd, "@"); idx != -1 {
					cmd = cmd[:idx]
				}
				go b.handleCommand(u.Message.Chat.ID, cmd)
			}
		}
	}
}

// ============================================================================
// HELPERS
// ============================================================================

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func parseNumber(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.Replace(s, "%", "", -1)
	s = strings.Replace(s, "K", "", -1)
	s = strings.Replace(s, "M", "", -1)
	s = strings.Replace(s, "B", "", -1)
	s = strings.Replace(s, "T", "", -1)
	s = strings.Replace(s, ",", "", -1)

	var val float64
	fmt.Sscanf(s, "%f", &val)
	return val
}

func splitMessage(text string, maxLen int) []string {
	var chunks []string
	lines := strings.Split(text, "\n")
	current := ""

	for _, line := range lines {
		if len(current)+len(line)+1 > maxLen {
			if current != "" {
				chunks = append(chunks, current)
			}
			current = line
		} else {
			if current != "" {
				current += "\n"
			}
			current += line
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

// ============================================================================
// ENTRYPOINT
// ============================================================================

func main() {
	var err error
	WIB, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		WIB = time.FixedZone("WIB", 7*60*60)
	}

	if BOT_TOKEN == "" {
		log.Fatal("[FATAL] BOT_TOKEN env var is required")
	}
	if CHAT_ID == "" {
		log.Println("[WARN] CHAT_ID not set. Alerts disabled. Use /chatid to get your chat ID.")
	}

	bot := NewBot(BOT_TOKEN, CHAT_ID)
	bot.run()
}
