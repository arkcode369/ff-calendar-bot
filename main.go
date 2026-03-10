package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// CONFIG
// ============================================================================

var (
	BOT_TOKEN   = os.Getenv("BOT_TOKEN")
	CHAT_ID     = os.Getenv("CHAT_ID") // owner chat ID for VPS access
	FF_JSON_URL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"
	ALERT_MINS  = []int{30, 15, 5}
	WIB         *time.Location
	BOT_START   time.Time
)

func init() {
	WIB, _ = time.LoadLocation("Asia/Jakarta")
	BOT_START = time.Now()
}

// ============================================================================
// DATA MODELS
// ============================================================================

type FFEvent struct {
	Title    string `json:"title"`
	Country  string `json:"country"`
	Date     string `json:"date"`
	Impact   string `json:"impact"`
	Forecast string `json:"forecast"`
	Previous string `json:"previous"`
}

type EventState struct {
	AlertedMins map[int]bool
	ResultSent  bool
}

type Bot struct {
	token          string
	ownerID        string
	client         *http.Client
	events         []FFEvent
	eventState     map[string]*EventState
	mu             sync.RWMutex
	lastFetch      time.Time
	offset         int
	alertsOn       bool
	fetchCount     int
	fetchErrs      int
	rateLimitUntil time.Time
	lastEtag       string
	lastModified   string
	lastFetchErr   string
	lastErrCode    int
}

// ============================================================================
// TELEGRAM TYPES
// ============================================================================

type TGUpdate struct {
	UpdateID int         `json:"update_id"`
	Message  *TGMessage  `json:"message"`
	Callback *TGCallback `json:"callback_query"`
}
type TGMessage struct {
	MessageID int    `json:"message_id"`
	Chat      TGChat `json:"chat"`
	Text      string `json:"text"`
}
type TGChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type TGCallback struct {
	ID      string     `json:"id"`
	From    TGUser     `json:"from"`
	Message *TGMessage `json:"message"`
	Data    string     `json:"data"`
}
type TGUser struct {
	ID int64 `json:"id"`
}
type TGResponse struct {
	Ok     bool       `json:"ok"`
	Result []TGUpdate `json:"result"`
}
type TGSendResp struct {
	Ok   bool   `json:"ok"`
	Desc string `json:"description"`
}
type InlineBtn struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
}
type InlineKB struct {
	Keyboard [][]InlineBtn `json:"inline_keyboard"`
}

// ============================================================================
// COUNTRY FLAGS + IMPACT ICONS
// ============================================================================

var countryFlags = map[string]string{
	"USD": "\U0001F1FA\U0001F1F8", "EUR": "\U0001F1EA\U0001F1FA",
	"GBP": "\U0001F1EC\U0001F1E7", "JPY": "\U0001F1EF\U0001F1F5",
	"AUD": "\U0001F1E6\U0001F1FA", "NZD": "\U0001F1F3\U0001F1FF",
	"CAD": "\U0001F1E8\U0001F1E6", "CHF": "\U0001F1E8\U0001F1ED",
	"CNY": "\U0001F1E8\U0001F1F3",
}

func flag(c string) string {
	if f, ok := countryFlags[strings.ToUpper(c)]; ok {
		return f
	}
	return "\U0001F30D"
}

func impactIcon(imp string) string {
	switch strings.ToLower(imp) {
	case "high":
		return "\U0001F534"
	case "medium":
		return "\U0001F7E0"
	case "low":
		return "\U0001F7E1"
	case "holiday":
		return "\U0001F3D6"
	default:
		return "\u26AA"
	}
}

// ============================================================================
// TIME PARSING — Fixed for ISO 8601 / RFC3339
// ============================================================================

func parseEventTime(dateStr string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05", dateStr)
	}
	return t, err
}

func eventKey(e FFEvent) string {
	return fmt.Sprintf("%s|%s|%s", e.Date, e.Country, e.Title)
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	days := int(d.Hours()) / 24
	hrs := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hrs)
	}
	if hrs > 0 {
		return fmt.Sprintf("%dh%dm", hrs, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// dataStatusBanner returns a warning line if data is stale or errored.
// Must be called with b.mu.RLock held or from a safe context.
func (b *Bot) dataStatusBanner() string {
	if b.lastFetchErr == "" && b.lastErrCode == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\u26A0\uFE0F <b>Data mungkin tidak terbaru</b>\n")
	if b.lastErrCode > 0 {
		sb.WriteString(fmt.Sprintf("\u2514 Error HTTP %d dari:\n  <code>%s</code>\n", b.lastErrCode, FF_JSON_URL))
	} else if b.lastFetchErr != "" {
		sb.WriteString(fmt.Sprintf("\u2514 %s\n", b.lastFetchErr))
	}
	if !b.rateLimitUntil.IsZero() && time.Now().Before(b.rateLimitUntil) {
		rem := time.Until(b.rateLimitUntil).Round(time.Second)
		sb.WriteString(fmt.Sprintf("\u23F3 Cooldown: %v lagi\n", rem))
	}
	if !b.lastFetch.IsZero() {
		age := fmtDuration(time.Since(b.lastFetch))
		sb.WriteString(fmt.Sprintf("\U0001F4BE Data cache: %s lalu (%d events)\n", age, len(b.events)))
	}
	return sb.String()
}

// ============================================================================
// BOT CORE
// ============================================================================

func NewBot(token, ownerID string) *Bot {
	return &Bot{
		token:      token,
		ownerID:    ownerID,
		client:     &http.Client{Timeout: 15 * time.Second},
		eventState: make(map[string]*EventState),
		alertsOn:   ownerID != "",
	}
}

func (b *Bot) api(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.token, method)
}

func (b *Bot) post(method string, p map[string]interface{}) error {
	data, _ := json.Marshal(p)
	req, err := http.NewRequest("POST", b.api(method), strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r TGSendResp
	json.NewDecoder(resp.Body).Decode(&r)
	if !r.Ok {
		return fmt.Errorf("tg %s: %s", method, r.Desc)
	}
	return nil
}

func (b *Bot) send(chatID, text string) error {
	return b.post("sendMessage", map[string]interface{}{
		"chat_id": chatID, "text": text, "parse_mode": "HTML",
		"disable_web_page_preview": true,
	})
}

func (b *Bot) sendKB(chatID, text string, kb InlineKB) error {
	return b.post("sendMessage", map[string]interface{}{
		"chat_id": chatID, "text": text, "parse_mode": "HTML",
		"reply_markup": kb, "disable_web_page_preview": true,
	})
}

func (b *Bot) edit(chatID string, msgID int, text string, kb *InlineKB) error {
	p := map[string]interface{}{
		"chat_id": chatID, "message_id": msgID, "text": text,
		"parse_mode": "HTML", "disable_web_page_preview": true,
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return b.post("editMessageText", p)
}

func (b *Bot) answerCB(cbID, text string) {
	p := map[string]interface{}{"callback_query_id": cbID}
	if text != "" {
		p["text"] = text
	}
	b.post("answerCallbackQuery", p)
}

func (b *Bot) getUpdates() ([]TGUpdate, error) {
	url := fmt.Sprintf("%s?offset=%d&timeout=1&allowed_updates=[\"message\",\"callback_query\"]",
		b.api("getUpdates"), b.offset)
	resp, err := b.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r TGResponse
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Result, nil
}

// ============================================================================
// DATA FETCHER
// ============================================================================

func (b *Bot) fetchEvents() error {
	// Cooldown: skip if we got rate-limited recently
	b.mu.RLock()
	cooldownUntil := b.rateLimitUntil
	b.mu.RUnlock()
	if time.Now().Before(cooldownUntil) {
		rem := time.Until(cooldownUntil).Round(time.Second)
		log.Printf("[FETCH] Rate-limit cooldown %v remaining, skipping (using cached data)", rem)
		return nil
	}

	const maxRetries = 3
	backoffs := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			wait := backoffs[attempt-1]
			log.Printf("[FETCH] Retry %d/%d in %v...", attempt+1, maxRetries, wait)
			time.Sleep(wait)
		}

		req, _ := http.NewRequest("GET", FF_JSON_URL, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		// Conditional fetch — skip download if data unchanged
		b.mu.RLock()
		if b.lastEtag != "" {
			req.Header.Set("If-None-Match", b.lastEtag)
		}
		if b.lastModified != "" {
			req.Header.Set("If-Modified-Since", b.lastModified)
		}
		b.mu.RUnlock()

		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = err
			b.mu.Lock()
			b.lastFetchErr = fmt.Sprintf("Connection error: %v", err)
			b.lastErrCode = 0
			b.mu.Unlock()
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		log.Printf("[FETCH] HTTP %d | %d bytes | URL: %s (attempt %d)", resp.StatusCode, len(data), FF_JSON_URL, attempt+1)

		// 304 Not Modified — data unchanged, no need to re-parse
		if resp.StatusCode == 304 {
			b.mu.Lock()
			b.lastFetch = time.Now()
			b.fetchCount++
			b.lastFetchErr = ""
			b.lastErrCode = 0
			b.mu.Unlock()
			log.Printf("[FETCH] 304 Not Modified — using cached %d events", len(b.events))
			return nil
		}

		// Rate limited — set cooldown, keep old data
		if resp.StatusCode == 429 {
			b.mu.Lock()
			b.rateLimitUntil = time.Now().Add(10 * time.Minute)
			b.fetchErrs++
			b.lastFetchErr = fmt.Sprintf("Rate limited (HTTP 429) from %s", FF_JSON_URL)
			b.lastErrCode = 429
			b.mu.Unlock()
			log.Printf("[FETCH] Rate limited! Cooling down 10min. Cached data preserved.")
			return fmt.Errorf("rate limited (429) from %s, cooldown 10min", FF_JSON_URL)
		}

		// Server error — retry
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("http %d from %s", resp.StatusCode, FF_JSON_URL)
			b.mu.Lock()
			b.lastFetchErr = fmt.Sprintf("Server error (HTTP %d) from %s", resp.StatusCode, FF_JSON_URL)
			b.lastErrCode = resp.StatusCode
			b.mu.Unlock()
			continue
		}

		// Other non-200
		if resp.StatusCode != 200 {
			b.mu.Lock()
			b.fetchErrs++
			b.lastFetchErr = fmt.Sprintf("HTTP %d from %s", resp.StatusCode, FF_JSON_URL)
			b.lastErrCode = resp.StatusCode
			b.mu.Unlock()
			return fmt.Errorf("http %d from %s", resp.StatusCode, FF_JSON_URL)
		}

		// Sanity check: JSON must start with '['
		trimmed := strings.TrimSpace(string(data))
		if len(trimmed) == 0 || trimmed[0] != '[' {
			lastErr = fmt.Errorf("non-JSON response from %s: %.80s", FF_JSON_URL, trimmed)
			b.mu.Lock()
			b.lastFetchErr = fmt.Sprintf("Invalid response from %s (not JSON)", FF_JSON_URL)
			b.lastErrCode = 200
			b.mu.Unlock()
			continue
		}

		var events []FFEvent
		if err := json.Unmarshal(data, &events); err != nil {
			b.mu.Lock()
			b.fetchErrs++
			b.lastFetchErr = fmt.Sprintf("JSON parse error from %s: %v", FF_JSON_URL, err)
			b.lastErrCode = 200
			b.mu.Unlock()
			return fmt.Errorf("json parse from %s: %w", FF_JSON_URL, err)
		}

		// Success — save data + cache headers
		b.mu.Lock()
		b.events = events
		b.lastFetch = time.Now()
		b.fetchCount++
		b.lastFetchErr = ""
		b.lastErrCode = 0
		if etag := resp.Header.Get("ETag"); etag != "" {
			b.lastEtag = etag
		}
		if lm := resp.Header.Get("Last-Modified"); lm != "" {
			b.lastModified = lm
		}
		for _, e := range events {
			k := eventKey(e)
			if _, ok := b.eventState[k]; !ok {
				b.eventState[k] = &EventState{AlertedMins: make(map[int]bool)}
			}
		}
		b.mu.Unlock()
		log.Printf("[FETCH] Loaded %d events (fresh data)", len(events))
		return nil
	}

	b.mu.Lock()
	b.fetchErrs++
	b.lastFetchErr = fmt.Sprintf("Failed after %d retries: %v", maxRetries, lastErr)
	b.lastErrCode = 0
	b.mu.Unlock()
	return fmt.Errorf("fetch failed after %d retries: %v", maxRetries, lastErr)
}

// ============================================================================
// INLINE KEYBOARDS
// ============================================================================

func kbMain() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F4C5 Today", Data: "today"}, {Text: "\U0001F4CB Week", Data: "week"}},
		{{Text: "\U0001F534 High Impact", Data: "high"}, {Text: "\u23ED Next", Data: "next"}},
		{{Text: "\u2699\uFE0F Settings", Data: "settings"}},
	}}
}

func kbCalendar() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F534 High Only", Data: "high"}, {Text: "\u23ED Next", Data: "next"}},
		{{Text: "\U0001F504 Refresh", Data: "today"}, {Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

func kbHigh() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F4C5 Full Calendar", Data: "today"}, {Text: "\u23ED Next", Data: "next"}},
		{{Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

func kbNext() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F4C5 Today", Data: "today"}, {Text: "\U0001F534 High Only", Data: "high"}},
		{{Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

func kbWeek() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F534 High Only", Data: "high"}, {Text: "\u23ED Next", Data: "next"}},
		{{Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

func (b *Bot) kbSettings(chatID string) InlineKB {
	alertText := "\U0001F514 Alerts ON"
	if !b.alertsOn {
		alertText = "\U0001F515 Alerts OFF"
	}
	rows := [][]InlineBtn{
		{{Text: alertText, Data: "toggle_alerts"}},
		{{Text: "\U0001F4CA Service Health", Data: "health"}},
	}
	// Owner-only: VPS Monitor
	if chatID == b.ownerID {
		rows = append(rows, []InlineBtn{{Text: "\U0001F5A5 VPS Monitor", Data: "vps"}})
	}
	rows = append(rows, []InlineBtn{{Text: "\U0001F519 Back", Data: "menu"}})
	return InlineKB{Keyboard: rows}
}

func kbVPS() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F504 Refresh", Data: "vps"}, {Text: "\U0001F433 Docker", Data: "vps_docker"}},
		{{Text: "\U0001F4E1 Network", Data: "vps_net"}, {Text: "\U0001F4BE Disk", Data: "vps_disk"}},
		{{Text: "\U0001F51D Top Procs", Data: "vps_top"}, {Text: "\U0001F4DD Logs", Data: "vps_logs"}},
		{{Text: "\u2699\uFE0F Settings", Data: "settings"}, {Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

func kbVPSSub() InlineKB {
	return InlineKB{Keyboard: [][]InlineBtn{
		{{Text: "\U0001F519 VPS Overview", Data: "vps"}, {Text: "\U0001F3E0 Menu", Data: "menu"}},
	}}
}

// ============================================================================
// EVENT FORMATTERS
// ============================================================================

func fmtEventLine(e FFEvent) string {
	t, err := parseEventTime(e.Date)
	if err != nil {
		return ""
	}
	wt := t.In(WIB)
	line := fmt.Sprintf("%s <b>%s</b> %s %s", impactIcon(e.Impact), wt.Format("15:04"), flag(e.Country), e.Title)
	var parts []string
	if e.Forecast != "" {
		parts = append(parts, "F: "+e.Forecast)
	}
	if e.Previous != "" {
		parts = append(parts, "P: "+e.Previous)
	}
	if len(parts) > 0 {
		line += "\n         " + strings.Join(parts, "  |  ")
	}
	return line
}

func (b *Bot) buildToday() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := time.Now().In(WIB)
	todayStr := now.Format("2006-01-02")

	var today []FFEvent
	var highCount int
	for _, e := range b.events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.In(WIB).Format("2006-01-02") == todayStr {
			today = append(today, e)
			if strings.EqualFold(e.Impact, "high") {
				highCount++
			}
		}
	}

	if len(today) == 0 {
		return fmt.Sprintf("\U0001F4C5 <b>%s</b>\n\nNo events scheduled today.", now.Format("Mon, 02 Jan 2006"))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\U0001F4C5 <b>%s (WIB)</b>\n", now.Format("Mon, 02 Jan 2006")))
	sb.WriteString(fmt.Sprintf("%d events | \U0001F534 %d high impact\n\n", len(today), highCount))

	for _, e := range today {
		line := fmtEventLine(e)
		if line != "" {
			sb.WriteString(line + "\n\n")
		}
	}

	sb.WriteString(fmt.Sprintf("\U0001F553 Updated: %s WIB", b.lastFetch.In(WIB).Format("15:04")))
	if banner := b.dataStatusBanner(); banner != "" {
		sb.WriteString("\n" + banner)
	}
	return sb.String()
}

func (b *Bot) buildWeek() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	type dayGroup struct {
		date   string
		label  string
		events []FFEvent
	}

	groups := make(map[string]*dayGroup)
	var order []string

	for _, e := range b.events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		wt := t.In(WIB)
		key := wt.Format("2006-01-02")
		if _, ok := groups[key]; !ok {
			groups[key] = &dayGroup{date: key, label: wt.Format("Mon, 02 Jan")}
			order = append(order, key)
		}
		groups[key].events = append(groups[key].events, e)
	}
	sort.Strings(order)

	var sb strings.Builder
	sb.WriteString("\U0001F4CB <b>WEEK OVERVIEW (WIB)</b>\n\n")

	for _, key := range order {
		g := groups[key]
		var hi int
		for _, e := range g.events {
			if strings.EqualFold(e.Impact, "high") {
				hi++
			}
		}
		sb.WriteString(fmt.Sprintf("<b>%s</b>  (%d events", g.label, len(g.events)))
		if hi > 0 {
			sb.WriteString(fmt.Sprintf(", \U0001F534 %d", hi))
		}
		sb.WriteString(")\n")
		for _, e := range g.events {
			line := fmtEventLine(e)
			if line != "" {
				sb.WriteString(line + "\n")
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("\U0001F553 Updated: %s WIB", b.lastFetch.In(WIB).Format("15:04")))
	if banner := b.dataStatusBanner(); banner != "" {
		sb.WriteString("\n" + banner)
	}
	return sb.String()
}

func (b *Bot) buildHigh() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	type dayGroup struct {
		label  string
		events []FFEvent
	}
	groups := make(map[string]*dayGroup)
	var order []string
	var total int

	for _, e := range b.events {
		if !strings.EqualFold(e.Impact, "high") {
			continue
		}
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		wt := t.In(WIB)
		key := wt.Format("2006-01-02")
		if _, ok := groups[key]; !ok {
			groups[key] = &dayGroup{label: wt.Format("Mon, 02 Jan")}
			order = append(order, key)
		}
		groups[key].events = append(groups[key].events, e)
		total++
	}
	sort.Strings(order)

	if total == 0 {
		return "\U0001F534 <b>HIGH IMPACT</b>\n\nNo high-impact events this week."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\U0001F534 <b>HIGH IMPACT EVENTS (WIB)</b>\n%d events this week\n\n", total))

	for _, key := range order {
		g := groups[key]
		sb.WriteString(fmt.Sprintf("<b>%s</b>\n", g.label))
		for _, e := range g.events {
			line := fmtEventLine(e)
			if line != "" {
				sb.WriteString(line + "\n")
			}
		}
		sb.WriteString("\n")
	}

	sb.WriteString(fmt.Sprintf("\U0001F553 Updated: %s WIB", b.lastFetch.In(WIB).Format("15:04")))
	if banner := b.dataStatusBanner(); banner != "" {
		sb.WriteString("\n" + banner)
	}
	return sb.String()
}

func (b *Bot) buildNext() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := time.Now()
	type upcoming struct {
		event FFEvent
		at    time.Time
	}
	var ups []upcoming

	for _, e := range b.events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.After(now) {
			ups = append(ups, upcoming{e, t})
		}
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].at.Before(ups[j].at) })

	if len(ups) == 0 {
		return "\u23ED <b>NEXT EVENTS</b>\n\nNo upcoming events."
	}

	limit := 8
	if len(ups) < limit {
		limit = len(ups)
	}

	var sb strings.Builder
	sb.WriteString("\u23ED <b>NEXT EVENTS</b>\n\n")

	for _, u := range ups[:limit] {
		wt := u.at.In(WIB)
		diff := u.at.Sub(now)
		line := fmt.Sprintf("%s <b>%s</b> %s %s  <i>(%s)</i>",
			impactIcon(u.event.Impact), wt.Format("15:04"), flag(u.event.Country),
			u.event.Title, fmtDuration(diff))
		var parts []string
		if u.event.Forecast != "" {
			parts = append(parts, "F: "+u.event.Forecast)
		}
		if u.event.Previous != "" {
			parts = append(parts, "P: "+u.event.Previous)
		}
		if len(parts) > 0 {
			line += "\n         " + strings.Join(parts, "  |  ")
		}
		sb.WriteString(line + "\n\n")
	}

	if len(ups) > limit {
		sb.WriteString(fmt.Sprintf("<i>+%d more events...</i>\n", len(ups)-limit))
	}
	return sb.String()
}

// ============================================================================
// ALERT ENGINE
// ============================================================================

func (b *Bot) checkAlerts() {
	if !b.alertsOn || b.ownerID == "" {
		return
	}
	b.mu.RLock()
	events := b.events
	b.mu.RUnlock()
	now := time.Now()

	for _, e := range events {
		if !strings.EqualFold(e.Impact, "high") {
			continue
		}
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		key := eventKey(e)
		minsUntil := int(t.Sub(now).Minutes())

		b.mu.RLock()
		state := b.eventState[key]
		b.mu.RUnlock()
		if state == nil {
			continue
		}

		// Pre-event alerts
		for _, m := range ALERT_MINS {
			if minsUntil <= m && minsUntil > m-2 && !state.AlertedMins[m] {
				wt := t.In(WIB)
				msg := fmt.Sprintf("\u23F0 <b>%d min to go!</b>\n\n%s <b>%s</b> %s %s",
					m, impactIcon(e.Impact), wt.Format("15:04"), flag(e.Country), e.Title)
				if e.Forecast != "" {
					msg += "\nForecast: " + e.Forecast
				}
				if e.Previous != "" {
					msg += "\nPrevious: " + e.Previous
				}
				b.send(b.ownerID, msg)
				b.mu.Lock()
				state.AlertedMins[m] = true
				b.mu.Unlock()
			}
		}
	}
}

// ============================================================================
// SETTINGS & HEALTH
// ============================================================================

func (b *Bot) buildSettings(chatID string) string {
	alertStatus := "\U0001F7E2 ON"
	if !b.alertsOn {
		alertStatus = "\U0001F534 OFF"
	}
	var sb strings.Builder
	sb.WriteString("\u2699\uFE0F <b>SETTINGS</b>\n\n")
	sb.WriteString(fmt.Sprintf("\U0001F514 Alerts: %s\n", alertStatus))
	sb.WriteString(fmt.Sprintf("\U0001F4CA Data source: ForexFactory\n"))
	sb.WriteString(fmt.Sprintf("\U0001F30D Timezone: WIB (UTC+7)\n"))
	if chatID == b.ownerID {
		sb.WriteString("\n\U0001F511 <i>Owner access: VPS Monitor available</i>")
	}
	return sb.String()
}

func (b *Bot) buildHealth() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	uptime := time.Since(BOT_START)
	var sb strings.Builder
	sb.WriteString("\U0001F4CA <b>SERVICE HEALTH</b>\n\n")

	// Bot status
	sb.WriteString("<b>Bot</b>\n")
	sb.WriteString(fmt.Sprintf("  \U0001F7E2 Status: Running\n"))
	sb.WriteString(fmt.Sprintf("  \u23F1 Uptime: %s\n", fmtDuration(uptime)))
	sb.WriteString(fmt.Sprintf("  \U0001F4E6 Go: %s\n\n", runtime.Version()))

	// Data status
	sb.WriteString("<b>Data Feed</b>\n")
	sb.WriteString(fmt.Sprintf("  \U0001F4C8 Events loaded: %d\n", len(b.events)))
	sb.WriteString(fmt.Sprintf("  \U0001F504 Fetch count: %d\n", b.fetchCount))
	sb.WriteString(fmt.Sprintf("  \u274C Fetch errors: %d\n", b.fetchErrs))
	if !b.lastFetch.IsZero() {
		sb.WriteString(fmt.Sprintf("  \U0001F553 Last fetch: %s WIB\n", b.lastFetch.In(WIB).Format("15:04:05")))
		sb.WriteString(fmt.Sprintf("  \u23F3 Data age: %s\n", fmtDuration(time.Since(b.lastFetch))))
	}
	if b.lastFetchErr != "" {
		sb.WriteString(fmt.Sprintf("  \u26A0\uFE0F Last error: %s\n", b.lastFetchErr))
	}
	if !b.rateLimitUntil.IsZero() && time.Now().Before(b.rateLimitUntil) {
		rem := time.Until(b.rateLimitUntil).Round(time.Second)
		sb.WriteString(fmt.Sprintf("  \U0001F6AB Rate limit cooldown: %v\n", rem))
	}
	if b.lastEtag != "" {
		sb.WriteString("  \U0001F4BE Cache: ETag active\n")
	}

	// Alert status
	sb.WriteString("\n<b>Alerts</b>\n")
	if b.alertsOn {
		sb.WriteString("  \U0001F7E2 Active\n")
	} else {
		sb.WriteString("  \U0001F534 Disabled\n")
	}
	sb.WriteString(fmt.Sprintf("  \u23F0 Intervals: %v min", ALERT_MINS))

	return sb.String()
}

// ============================================================================
// VPS MONITORING (Owner Only)
// ============================================================================

func shell(cmd string) string {
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\n%s", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func (b *Bot) isOwner(chatID string) bool {
	return chatID == b.ownerID && b.ownerID != ""
}

func (b *Bot) buildVPSOverview() string {
	var sb strings.Builder
	sb.WriteString("\U0001F5A5 <b>VPS MONITOR</b>\n\n")

	// Hostname & OS
	hostname := shell("hostname")
	osInfo := shell("cat /etc/os-release 2>/dev/null | grep PRETTY_NAME | cut -d'\"' -f2")
	if osInfo == "" {
		osInfo = shell("uname -o")
	}
	kernel := shell("uname -r")
	uptime := shell("uptime -p 2>/dev/null || uptime")

	sb.WriteString("<b>System</b>\n")
	sb.WriteString(fmt.Sprintf("  Host: <code>%s</code>\n", hostname))
	sb.WriteString(fmt.Sprintf("  OS: %s\n", osInfo))
	sb.WriteString(fmt.Sprintf("  Kernel: %s\n", kernel))
	sb.WriteString(fmt.Sprintf("  Uptime: %s\n\n", uptime))

	// CPU
	cpuModel := shell("grep 'model name' /proc/cpuinfo 2>/dev/null | head -1 | cut -d: -f2 | xargs")
	cpuCores := shell("nproc 2>/dev/null || echo '?'")
	loadAvg := shell("cat /proc/loadavg 2>/dev/null | awk '{print $1, $2, $3}'")
	// CPU usage from /proc/stat snapshot
	cpuUsage := shell(`top -bn1 2>/dev/null | grep "Cpu(s)" | awk '{print $2}' | cut -d'%' -f1`)
	if cpuUsage == "" {
		cpuUsage = shell(`grep 'cpu ' /proc/stat | awk '{usage=($2+$4)*100/($2+$4+$5)} END {printf "%.1f", usage}'`)
	}

	sb.WriteString("<b>CPU</b>\n")
	if cpuModel != "" {
		sb.WriteString(fmt.Sprintf("  Model: %s\n", cpuModel))
	}
	sb.WriteString(fmt.Sprintf("  Cores: %s\n", cpuCores))
	sb.WriteString(fmt.Sprintf("  Usage: %s%%\n", cpuUsage))
	sb.WriteString(fmt.Sprintf("  Load: %s\n\n", loadAvg))

	// Memory
	memTotal := shell("free -h | awk '/Mem:/{print $2}'")
	memUsed := shell("free -h | awk '/Mem:/{print $3}'")
	memFree := shell("free -h | awk '/Mem:/{print $4}'")
	memPct := shell("free | awk '/Mem:/{printf \"%.1f\", $3/$2*100}'")
	swapTotal := shell("free -h | awk '/Swap:/{print $2}'")
	swapUsed := shell("free -h | awk '/Swap:/{print $3}'")

	sb.WriteString("<b>Memory</b>\n")
	sb.WriteString(fmt.Sprintf("  RAM: %s / %s (%s%%)\n", memUsed, memTotal, memPct))
	sb.WriteString(fmt.Sprintf("  Free: %s\n", memFree))
	sb.WriteString(fmt.Sprintf("  Swap: %s / %s\n\n", swapUsed, swapTotal))

	// Disk summary
	diskInfo := shell("df -h / | awk 'NR==2{printf \"%s / %s (%s)\", $3, $2, $5}'")
	sb.WriteString("<b>Disk (/)</b>\n")
	sb.WriteString(fmt.Sprintf("  Used: %s\n\n", diskInfo))

	// Docker summary
	dockerCount := shell("docker ps -q 2>/dev/null | wc -l")
	dockerTotal := shell("docker ps -aq 2>/dev/null | wc -l")
	sb.WriteString("<b>Docker</b>\n")
	sb.WriteString(fmt.Sprintf("  Running: %s / %s containers\n\n", dockerCount, dockerTotal))

	// Network - primary interface
	primaryIP := shell("hostname -I 2>/dev/null | awk '{print $1}'")
	sb.WriteString("<b>Network</b>\n")
	sb.WriteString(fmt.Sprintf("  IP: <code>%s</code>\n\n", primaryIP))

	sb.WriteString(fmt.Sprintf("\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

func (b *Bot) buildVPSDocker() string {
	var sb strings.Builder
	sb.WriteString("\U0001F433 <b>DOCKER CONTAINERS</b>\n\n")

	// Running containers with stats
	containers := shell(`docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}" 2>/dev/null`)
	if containers == "" || strings.Contains(containers, "error") {
		sb.WriteString("Docker not available or no containers running.\n")
		return sb.String()
	}
	sb.WriteString("<b>Running:</b>\n")
	sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n\n", containers))

	// Resource usage
	stats := shell(`docker stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}" 2>/dev/null`)
	if stats != "" {
		sb.WriteString("<b>Resource Usage:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n\n", stats))
	}

	// Stopped containers
	stopped := shell(`docker ps -f "status=exited" --format "{{.Names}}\t{{.Status}}" 2>/dev/null`)
	if stopped != "" {
		sb.WriteString("<b>Stopped:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n", stopped))
	}

	sb.WriteString(fmt.Sprintf("\n\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

func (b *Bot) buildVPSNetwork() string {
	var sb strings.Builder
	sb.WriteString("\U0001F4E1 <b>NETWORK</b>\n\n")

	// Interfaces
	interfaces := shell(`ip -br addr 2>/dev/null || ifconfig 2>/dev/null | grep -E "^[a-z]|inet "`)
	sb.WriteString("<b>Interfaces:</b>\n")
	sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n\n", interfaces))

	// Traffic stats
	traffic := shell(`cat /proc/net/dev 2>/dev/null | awk 'NR>2{if($2>0) printf "%-10s RX: %10.1f MB  TX: %10.1f MB\n", $1, $2/1048576, $10/1048576}'`)
	if traffic != "" {
		sb.WriteString("<b>Traffic:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n\n", traffic))
	}

	// Listening ports
	ports := shell(`ss -tlnp 2>/dev/null | head -15 || netstat -tlnp 2>/dev/null | head -15`)
	if ports != "" {
		sb.WriteString("<b>Listening Ports:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n", ports))
	}

	sb.WriteString(fmt.Sprintf("\n\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

func (b *Bot) buildVPSDisk() string {
	var sb strings.Builder
	sb.WriteString("\U0001F4BE <b>DISK USAGE</b>\n\n")

	// Filesystem usage
	df := shell("df -h --output=target,size,used,avail,pcent 2>/dev/null | head -20")
	if df == "" {
		df = shell("df -h | head -20")
	}
	sb.WriteString("<b>Filesystems:</b>\n")
	sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n\n", df))

	// Inode usage
	inodes := shell("df -ih / | awk 'NR==2{printf \"Used: %s / %s (%s)\", $3, $2, $5}'")
	sb.WriteString("<b>Inodes (/):</b>\n")
	sb.WriteString(fmt.Sprintf("  %s\n\n", inodes))

	// Docker disk usage
	dockerDisk := shell("docker system df 2>/dev/null")
	if dockerDisk != "" && !strings.Contains(dockerDisk, "error") {
		sb.WriteString("<b>Docker Disk:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n", dockerDisk))
	}

	// Largest dirs
	largest := shell("du -sh /var/log /tmp /var/lib/docker 2>/dev/null | sort -rh | head -5")
	if largest != "" {
		sb.WriteString("\n<b>Large Directories:</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n", largest))
	}

	sb.WriteString(fmt.Sprintf("\n\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

func (b *Bot) buildVPSTop() string {
	var sb strings.Builder
	sb.WriteString("\U0001F51D <b>TOP PROCESSES</b>\n\n")

	// Top by CPU
	topCPU := shell(`ps aux --sort=-%cpu 2>/dev/null | head -11 | awk '{printf "%-6s %5s%% %5s%% %s\n", $2, $3, $4, $11}' | head -11`)
	sb.WriteString("<b>By CPU:</b>\n")
	sb.WriteString("<pre>PID    CPU%   MEM%  CMD\n")
	sb.WriteString(topCPU + "</pre>\n\n")

	// Top by Memory
	topMem := shell(`ps aux --sort=-%mem 2>/dev/null | head -11 | awk '{printf "%-6s %5s%% %5s%% %s\n", $2, $3, $4, $11}' | head -11`)
	sb.WriteString("<b>By Memory:</b>\n")
	sb.WriteString("<pre>PID    CPU%   MEM%  CMD\n")
	sb.WriteString(topMem + "</pre>\n\n")

	// Process count
	procCount := shell("ps aux 2>/dev/null | wc -l")
	sb.WriteString(fmt.Sprintf("Total processes: %s\n", procCount))

	sb.WriteString(fmt.Sprintf("\n\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

func (b *Bot) buildVPSLogs() string {
	var sb strings.Builder
	sb.WriteString("\U0001F4DD <b>RECENT LOGS</b>\n\n")

	// Bot's own container logs
	logs := shell("docker logs --tail 25 $(docker ps -q --filter ancestor=$(docker images -q --filter reference='*ff*calendar*' 2>/dev/null | head -1) 2>/dev/null | head -1) 2>&1 | tail -25")
	if logs == "" || strings.Contains(logs, "error") {
		// fallback: try by name
		logs = shell("docker logs --tail 25 ff-calendar-bot 2>&1 || docker logs --tail 25 $(docker ps --format '{{.Names}}' | head -1) 2>&1 | tail -25")
	}
	if logs != "" && !strings.Contains(logs, "error") {
		sb.WriteString("<b>Container Logs:</b>\n")
		// Truncate long lines
		lines := strings.Split(logs, "\n")
		for _, l := range lines {
			if len(l) > 120 {
				l = l[:120] + "..."
			}
			sb.WriteString(l + "\n")
		}
	} else {
		sb.WriteString("No container logs available.\n")
	}

	// System journal (last few)
	syslog := shell("journalctl --no-pager -n 10 --priority=err 2>/dev/null")
	if syslog != "" && !strings.Contains(syslog, "No journal") {
		sb.WriteString("\n<b>System Errors (last 10):</b>\n")
		sb.WriteString(fmt.Sprintf("<pre>%s</pre>\n", syslog))
	}

	sb.WriteString(fmt.Sprintf("\n\U0001F553 %s WIB", time.Now().In(WIB).Format("15:04:05")))
	return sb.String()
}

// ============================================================================
// COMMAND HANDLER
// ============================================================================

func (b *Bot) handleCommand(chatID string, text string) {
	cid := fmt.Sprintf("%d", 0) // placeholder
	_ = cid
	switch {
	case text == "/start" || text == "/menu":
		msg := "\U0001F4B9 <b>FF Calendar Bot</b>\n\n"
		msg += "Forex economic calendar with real-time alerts.\n"
		msg += "Data from ForexFactory, times in WIB.\n\n"
		msg += "Choose an option:"
		b.sendKB(chatID, msg, kbMain())

	case text == "/today" || text == "/calendar":
		b.sendKB(chatID, b.buildToday(), kbCalendar())

	case text == "/week":
		b.sendKB(chatID, b.buildWeek(), kbWeek())

	case text == "/high":
		b.sendKB(chatID, b.buildHigh(), kbHigh())

	case text == "/next":
		b.sendKB(chatID, b.buildNext(), kbNext())

	case text == "/settings":
		b.sendKB(chatID, b.buildSettings(chatID), b.kbSettings(chatID))

	case text == "/health":
		b.sendKB(chatID, b.buildHealth(), InlineKB{Keyboard: [][]InlineBtn{
			{{Text: "\u2699\uFE0F Settings", Data: "settings"}, {Text: "\U0001F3E0 Menu", Data: "menu"}},
		}})

	case text == "/vps":
		if !b.isOwner(chatID) {
			b.send(chatID, "\U0001F512 Owner access only.")
			return
		}
		b.sendKB(chatID, b.buildVPSOverview(), kbVPS())

	default:
		b.sendKB(chatID, "Unknown command. Use /start to see the menu.", kbMain())
	}
}

// ============================================================================
// CALLBACK HANDLER
// ============================================================================

func (b *Bot) handleCallback(cb *TGCallback) {
	if cb == nil || cb.Message == nil {
		return
	}
	chatID := fmt.Sprintf("%d", cb.Message.Chat.ID)
	msgID := cb.Message.MessageID
	data := cb.Data

	var text string
	var kb *InlineKB

	switch data {
	case "menu":
		text = "\U0001F4B9 <b>FF Calendar Bot</b>\n\n"
		text += "Forex economic calendar with real-time alerts.\n"
		text += "Data from ForexFactory, times in WIB.\n\n"
		text += "Choose an option:"
		k := kbMain()
		kb = &k

	case "today":
		text = b.buildToday()
		k := kbCalendar()
		kb = &k

	case "week":
		text = b.buildWeek()
		k := kbWeek()
		kb = &k

	case "high":
		text = b.buildHigh()
		k := kbHigh()
		kb = &k

	case "next":
		text = b.buildNext()
		k := kbNext()
		kb = &k

	case "settings":
		text = b.buildSettings(chatID)
		k := b.kbSettings(chatID)
		kb = &k

	case "toggle_alerts":
		b.alertsOn = !b.alertsOn
		status := "ON \U0001F7E2"
		if !b.alertsOn {
			status = "OFF \U0001F534"
		}
		b.answerCB(cb.ID, "Alerts: "+status)
		text = b.buildSettings(chatID)
		k := b.kbSettings(chatID)
		kb = &k

	case "health":
		text = b.buildHealth()
		k := InlineKB{Keyboard: [][]InlineBtn{
			{{Text: "\u2699\uFE0F Settings", Data: "settings"}, {Text: "\U0001F3E0 Menu", Data: "menu"}},
		}}
		kb = &k

	case "vps":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSOverview()
		k := kbVPS()
		kb = &k

	case "vps_docker":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSDocker()
		k := kbVPSSub()
		kb = &k

	case "vps_net":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSNetwork()
		k := kbVPSSub()
		kb = &k

	case "vps_disk":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSDisk()
		k := kbVPSSub()
		kb = &k

	case "vps_top":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSTop()
		k := kbVPSSub()
		kb = &k

	case "vps_logs":
		if !b.isOwner(chatID) {
			b.answerCB(cb.ID, "\U0001F512 Owner only")
			return
		}
		text = b.buildVPSLogs()
		k := kbVPSSub()
		kb = &k

	default:
		b.answerCB(cb.ID, "Unknown action")
		return
	}

	if text != "" {
		err := b.edit(chatID, msgID, text, kb)
		if err != nil {
			// If edit fails (message not modified), send new
			if kb != nil {
				b.sendKB(chatID, text, *kb)
			} else {
				b.send(chatID, text)
			}
		}
	}
	b.answerCB(cb.ID, "")
}

// ============================================================================
// MAIN LOOP
// ============================================================================

func (b *Bot) run() {
	log.Println("[BOT] Starting FF Calendar Bot v3...")
	log.Printf("[BOT] Owner ID: %s", b.ownerID)

	if err := b.fetchEvents(); err != nil {
		log.Printf("[BOT] Initial fetch error: %v", err)
	}

	// Data refresh goroutine
	go func() {
		for {
			interval := 15 * time.Minute
			b.mu.RLock()
			for _, e := range b.events {
				t, err := parseEventTime(e.Date)
				if err == nil {
					diff := time.Until(t)
					if diff > 0 && diff < 35*time.Minute {
						interval = 5 * time.Minute
						break
					}
				}
			}
			b.mu.RUnlock()
			time.Sleep(interval)
			if err := b.fetchEvents(); err != nil {
				log.Printf("[FETCH] Error: %v", err)
			}
		}
	}()

	// Alert check goroutine
	go func() {
		for {
			time.Sleep(30 * time.Second)
			b.checkAlerts()
		}
	}()

	// Polling loop
	for {
		updates, err := b.getUpdates()
		if err != nil {
			log.Printf("[POLL] Error: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			b.offset = u.UpdateID + 1

			if u.Callback != nil {
				b.handleCallback(u.Callback)
				continue
			}

			if u.Message != nil && u.Message.Text != "" {
				chatID := fmt.Sprintf("%d", u.Message.Chat.ID)
				text := strings.TrimSpace(u.Message.Text)
				// Strip @botname from commands in groups
				if i := strings.Index(text, "@"); i > 0 {
					text = text[:i]
				}
				b.handleCommand(chatID, text)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ============================================================================
// ENTRYPOINT
// ============================================================================

func main() {
	if BOT_TOKEN == "" {
		log.Fatal("[FATAL] BOT_TOKEN not set")
	}
	if CHAT_ID == "" {
		log.Println("[WARN] CHAT_ID not set - alerts & VPS monitor disabled")
	}

	// Suppress unused import warnings
	_ = strconv.Itoa
	_ = sort.Strings

	bot := NewBot(BOT_TOKEN, CHAT_ID)
	bot.run()
}
