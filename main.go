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

// ── CONFIG ──────────────────────────────────────────────────────────────────

var (
	BOT_TOKEN   = os.Getenv("BOT_TOKEN")
	CHAT_ID     = os.Getenv("CHAT_ID")
	FF_JSON_URL = "https://nfs.faireconomy.media/ff_calendar_thisweek.json"
	ALERT_BEFORE = []int{30, 15, 5}
	WIB         *time.Location
)

// ── DATA ────────────────────────────────────────────────────────────────────

type FFEvent struct {
	Title    string `json:"title"`
	Country  string `json:"country"`
	Date     string `json:"date"`
	Impact   string `json:"impact"`
	Forecast string `json:"forecast"`
	Previous string `json:"previous"`
	Actual   string `json:"actual"`
}

type EventState struct {
	AlertedMinutes map[int]bool
	ActualSent     bool
}

type Bot struct {
	token      string
	chatID     string
	client     *http.Client
	events     []FFEvent
	eventState map[string]*EventState
	mu         sync.RWMutex
	lastFetch  time.Time
	offset     int
	alertsOn   bool
}

// ── TELEGRAM STRUCTS ────────────────────────────────────────────────────────

type TGUpdate struct {
	UpdateID      int         `json:"update_id"`
	Message       *TGMessage  `json:"message"`
	CallbackQuery *TGCallback `json:"callback_query"`
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

type TGSendResponse struct {
	Ok          bool   `json:"ok"`
	Description string `json:"description"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// ── COUNTRY FLAGS ───────────────────────────────────────────────────────────

var countryFlags = map[string]string{
	"USD": "\U0001F1FA\U0001F1F8", "EUR": "\U0001F1EA\U0001F1FA",
	"GBP": "\U0001F1EC\U0001F1E7", "JPY": "\U0001F1EF\U0001F1F5",
	"AUD": "\U0001F1E6\U0001F1FA", "NZD": "\U0001F1F3\U0001F1FF",
	"CAD": "\U0001F1E8\U0001F1E6", "CHF": "\U0001F1E8\U0001F1ED",
	"CNY": "\U0001F1E8\U0001F1F3", "ALL": "\U0001F30D",
}

func flag(country string) string {
	if f, ok := countryFlags[strings.ToUpper(country)]; ok {
		return f
	}
	return "\U0001F30D"
}

func impactIcon(impact string) string {
	switch strings.ToLower(impact) {
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

// ── BOT CORE ────────────────────────────────────────────────────────────────

func NewBot(token, chatID string) *Bot {
	return &Bot{
		token:      token,
		chatID:     chatID,
		client:     &http.Client{Timeout: 15 * time.Second},
		eventState: make(map[string]*EventState),
		alertsOn:   chatID != "",
	}
}

func (b *Bot) apiURL(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", b.token, method)
}

func (b *Bot) postJSON(method string, payload map[string]interface{}) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", b.apiURL(method), strings.NewReader(string(body)))
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
		return fmt.Errorf("tg %s: %s", method, result.Description)
	}
	return nil
}

func (b *Bot) sendMessage(chatID, text string) error {
	if chatID == "" {
		chatID = b.chatID
	}
	return b.postJSON("sendMessage", map[string]interface{}{
		"chat_id": chatID, "text": text, "parse_mode": "HTML",
		"disable_web_page_preview": true,
	})
}

func (b *Bot) sendWithKB(chatID, text string, kb InlineKeyboardMarkup) error {
	if chatID == "" {
		chatID = b.chatID
	}
	return b.postJSON("sendMessage", map[string]interface{}{
		"chat_id": chatID, "text": text, "parse_mode": "HTML",
		"reply_markup": kb, "disable_web_page_preview": true,
	})
}

func (b *Bot) editMsg(chatID string, msgID int, text string, kb *InlineKeyboardMarkup) error {
	p := map[string]interface{}{
		"chat_id": chatID, "message_id": msgID, "text": text,
		"parse_mode": "HTML", "disable_web_page_preview": true,
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	return b.postJSON("editMessageText", p)
}

func (b *Bot) answerCB(cbID, text string) {
	p := map[string]interface{}{"callback_query_id": cbID}
	if text != "" {
		p["text"] = text
	}
	b.postJSON("answerCallbackQuery", p)
}

func (b *Bot) getUpdates() ([]TGUpdate, error) {
	url := fmt.Sprintf("%s?offset=%d&timeout=1&allowed_updates=[\"message\",\"callback_query\"]",
		b.apiURL("getUpdates"), b.offset)
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

// ── DATA FETCHER ────────────────────────────────────────────────────────────

func (b *Bot) fetchEvents() error {
	req, _ := http.NewRequest("GET", FF_JSON_URL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
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
		return fmt.Errorf("json: %w (status %d)", err, resp.StatusCode)
	}
	b.mu.Lock()
	b.events = events
	b.lastFetch = time.Now()
	for _, e := range events {
		key := eventKey(e)
		if _, ok := b.eventState[key]; !ok {
			b.eventState[key] = &EventState{AlertedMinutes: make(map[int]bool)}
		}
	}
	b.mu.Unlock()
	log.Printf("[FETCH] %d events", len(events))
	return nil
}

func eventKey(e FFEvent) string { return e.Title + "|" + e.Date }

func parseEventTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05-0700", s)
	}
	return t, err
}

// ── INLINE KEYBOARDS ────────────────────────────────────────────────────────

func kbMain() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F4C5 Today", CallbackData: "calendar"}, {Text: "\U0001F4CB Week", CallbackData: "week"}},
		{{Text: "\U0001F534 High Impact", CallbackData: "high"}, {Text: "\u23ED Next", CallbackData: "next"}},
		{{Text: "\u2139\uFE0F Status", CallbackData: "alerts"}, {Text: "\U0001F194 Chat ID", CallbackData: "chatid"}},
	}}
}

func kbCalendar() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F534 High Only", CallbackData: "high"}, {Text: "\u23ED Next", CallbackData: "next"}, {Text: "\U0001F504 Refresh", CallbackData: "refresh"}},
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

func kbHigh() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F4C5 Full Calendar", CallbackData: "calendar"}, {Text: "\u23ED Next", CallbackData: "next"}},
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

func kbNext() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F4C5 Today", CallbackData: "calendar"}, {Text: "\U0001F534 High Only", CallbackData: "high"}},
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

func kbWeek() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F534 High Only", CallbackData: "high"}, {Text: "\U0001F4C5 Today", CallbackData: "calendar"}},
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

func kbBack() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

func kbAlerts(on bool) InlineKeyboardMarkup {
	txt, data := "\u25B6\uFE0F Turn ON", "alerts_on"
	if on {
		txt, data = "\u23F8 Turn OFF", "alerts_off"
	}
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: txt, CallbackData: data}},
		{{Text: "\U0001F3E0 Menu", CallbackData: "start"}},
	}}
}

// ── FORMAT HELPERS ───────────────────────────────────────────────────────────

func fmtEventLine(e FFEvent) string {
	t, err := parseEventTime(e.Date)
	if err != nil {
		return ""
	}
	wt := t.In(WIB)
	line := fmt.Sprintf("%s <b>%s</b> %s %s", impactIcon(e.Impact), wt.Format("15:04"), flag(e.Country), e.Title)
	var dp []string
	if e.Forecast != "" {
		dp = append(dp, "F:"+e.Forecast)
	}
	if e.Previous != "" {
		dp = append(dp, "P:"+e.Previous)
	}
	if e.Actual != "" {
		dp = append(dp, "<b>A:"+e.Actual+"</b>")
	}
	if len(dp) > 0 {
		line += "\n    " + strings.Join(dp, " | ")
	}
	return line
}

func fmtToday(events []FFEvent) string {
	now := time.Now().In(WIB)
	dayStr := now.Format("2006-01-02")
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
		return fmt.Sprintf("\U0001F4C5 <b>%s</b>\n\nNo events today.", now.Format("Mon, 02 Jan 2006"))
	}
	sort.Slice(dayEvents, func(i, j int) bool { return dayEvents[i].Date < dayEvents[j].Date })
	high := 0
	for _, e := range dayEvents {
		if strings.EqualFold(e.Impact, "high") {
			high++
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\U0001F4C5 <b>%s</b>\n", now.Format("Mon, 02 Jan 2006")))
	sb.WriteString(fmt.Sprintf("%d events \u2022 %d high impact\n\n", len(dayEvents), high))
	for _, e := range dayEvents {
		if line := fmtEventLine(e); line != "" {
			sb.WriteString(line + "\n")
		}
	}
	sb.WriteString(fmt.Sprintf("\n<i>\U0001F534=HIGH \U0001F7E0=MED \U0001F7E1=LOW | %s</i>", time.Now().In(WIB).Format("15:04")))
	return sb.String()
}

func fmtWeek(events []FFEvent) string {
	dayMap := make(map[string][]FFEvent)
	var days []string
	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		ds := t.In(WIB).Format("2006-01-02")
		if _, ok := dayMap[ds]; !ok {
			days = append(days, ds)
		}
		dayMap[ds] = append(dayMap[ds], e)
	}
	sort.Strings(days)
	var sb strings.Builder
	sb.WriteString("\U0001F4CB <b>Weekly Calendar</b>\n\n")
	for _, ds := range days {
		dev := dayMap[ds]
		sort.Slice(dev, func(i, j int) bool { return dev[i].Date < dev[j].Date })
		dt, _ := time.Parse("2006-01-02", ds)
		h := 0
		for _, e := range dev {
			if strings.EqualFold(e.Impact, "high") {
				h++
			}
		}
		sb.WriteString(fmt.Sprintf("\U0001F4CC <b>%s</b> (%d", dt.Format("Mon 02 Jan"), len(dev)))
		if h > 0 {
			sb.WriteString(fmt.Sprintf(", %d\U0001F534", h))
		}
		sb.WriteString(")\n")
		for _, e := range dev {
			if line := fmtEventLine(e); line != "" {
				sb.WriteString(line + "\n")
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString("<i>Source: ForexFactory</i>")
	return sb.String()
}

func fmtHigh(events []FFEvent) string {
	var hi []FFEvent
	for _, e := range events {
		if strings.EqualFold(e.Impact, "high") {
			hi = append(hi, e)
		}
	}
	if len(hi) == 0 {
		return "\U0001F534 <b>High Impact</b>\n\nNo high-impact events this week."
	}
	sort.Slice(hi, func(i, j int) bool { return hi[i].Date < hi[j].Date })
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\U0001F534 <b>High Impact</b> (%d events)\n\n", len(hi)))
	curDay := ""
	for _, e := range hi {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		ds := t.In(WIB).Format("Mon 02 Jan")
		if ds != curDay {
			if curDay != "" {
				sb.WriteString("\n")
			}
			sb.WriteString(fmt.Sprintf("\U0001F4CC <b>%s</b>\n", ds))
			curDay = ds
		}
		wt := t.In(WIB)
		sb.WriteString(fmt.Sprintf("\U0001F534 <b>%s</b> %s %s\n", wt.Format("15:04"), flag(e.Country), e.Title))
		var dp []string
		if e.Forecast != "" {
			dp = append(dp, "F:"+e.Forecast)
		}
		if e.Previous != "" {
			dp = append(dp, "P:"+e.Previous)
		}
		if e.Actual != "" {
			dp = append(dp, "<b>A:"+e.Actual+"</b>")
		}
		if len(dp) > 0 {
			sb.WriteString("    " + strings.Join(dp, " | ") + "\n")
		}
	}
	sb.WriteString("\n<i>Source: ForexFactory</i>")
	return sb.String()
}

func fmtNext(events []FFEvent, count int) string {
	now := time.Now().In(WIB)
	var up []FFEvent
	for _, e := range events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.In(WIB).After(now) {
			up = append(up, e)
		}
	}
	sort.Slice(up, func(i, j int) bool { return up[i].Date < up[j].Date })
	if len(up) == 0 {
		return "\u23ED <b>Next Events</b>\n\nNo upcoming events this week."
	}
	if count > len(up) {
		count = len(up)
	}
	up = up[:count]
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("\u23ED <b>Next %d Events</b>\n\n", count))
	for _, e := range up {
		t, _ := parseEventTime(e.Date)
		wt := t.In(WIB)
		mins := int(time.Until(wt).Minutes())
		cd := ""
		if mins < 60 {
			cd = fmt.Sprintf("%dm", mins)
		} else if mins < 1440 {
			cd = fmt.Sprintf("%dh%dm", mins/60, mins%60)
		} else {
			cd = fmt.Sprintf("%dd%dh", mins/1440, (mins%1440)/60)
		}
		sb.WriteString(fmt.Sprintf("%s <b>%s</b> %s %s <i>(%s)</i>\n",
			impactIcon(e.Impact), wt.Format("15:04"), flag(e.Country), e.Title, cd))
		var dp []string
		if e.Forecast != "" {
			dp = append(dp, "F:"+e.Forecast)
		}
		if e.Previous != "" {
			dp = append(dp, "P:"+e.Previous)
		}
		if len(dp) > 0 {
			sb.WriteString("    " + strings.Join(dp, " | ") + "\n")
		}
	}
	sb.WriteString(fmt.Sprintf("\n<i>Updated %s</i>", time.Now().In(WIB).Format("15:04")))
	return sb.String()
}

func (b *Bot) fmtAlerts() string {
	now := time.Now()
	up := 0
	for _, e := range b.events {
		t, err := parseEventTime(e.Date)
		if err != nil {
			continue
		}
		if t.After(now) {
			up++
		}
	}
	b.mu.RLock()
	lf := b.lastFetch.In(WIB).Format("15:04:05")
	b.mu.RUnlock()
	st := "\u2705 ON"
	if !b.alertsOn {
		st = "\u274C OFF"
	}
	return fmt.Sprintf(
		"\u2139\uFE0F <b>Bot Status</b>\n\n"+
			"Alerts: %s\n"+
			"Events: %d loaded, %d upcoming\n"+
			"Refresh: %s WIB\n"+
			"Target: <code>%s</code>\n"+
			"Interval: 2min active / 5min idle",
		st, len(b.events), up, lf, b.chatID)
}

func fmtStart() string {
	return "\U0001F4B9 <b>FF Economic Calendar</b>\n\n" +
		"Real-time Forex Factory events\nwith auto-alerts before releases.\n\n" +
		"Pick an option \u2B07"
}

// ── ALERT ENGINE ─────────────────────────────────────────────────────────────

func (b *Bot) checkAlerts() {
	if !b.alertsOn || b.chatID == "" {
		return
	}
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

		// Pre-event alerts
		for _, am := range ALERT_BEFORE {
			if minsLeft <= am && minsLeft > (am-2) && !state.AlertedMinutes[am] {
				wt := t.In(WIB).Format("15:04")
				urgency := "\U0001F514" // bell
				if am <= 5 {
					urgency = "\U0001F6A8" // siren
				} else if am <= 15 {
					urgency = "\u26A0\uFE0F" // warning
				}

				msg := fmt.Sprintf(
					"%s <b>%dm before release</b>\n\n"+
						"%s %s %s\n"+
						"\u23F0 %s WIB",
					urgency, minsLeft,
					impactIcon(e.Impact), flag(e.Country), e.Title, wt)

				if e.Forecast != "" {
					msg += fmt.Sprintf("\nFcst: %s", e.Forecast)
				}
				if e.Previous != "" {
					msg += fmt.Sprintf("\nPrev: %s", e.Previous)
				}

				if err := b.sendMessage("", msg); err != nil {
					log.Printf("[ALERT] %v", err)
				} else {
					log.Printf("[ALERT] %dm: %s %s", am, e.Country, e.Title)
				}
				b.mu.Lock()
				state.AlertedMinutes[am] = true
				b.mu.Unlock()
			}
		}

		// Result alert
		if e.Actual != "" && !state.ActualSent && now.After(t) {
			wt := t.In(WIB).Format("15:04")
			verdict, vIcon := "", ""
			if e.Forecast != "" {
				av := parseNumber(e.Actual)
				fv := parseNumber(e.Forecast)
				if av > fv {
					verdict, vIcon = "BEAT", "\u2705"
				} else if av < fv {
					verdict, vIcon = "MISS", "\u274C"
				} else {
					verdict, vIcon = "IN LINE", "\u2796"
				}
			}

			msg := fmt.Sprintf(
				"\U0001F4CA <b>RESULT</b>\n\n"+
					"%s %s %s\n"+
					"\u23F0 %s WIB\n\n"+
					"Actual:   <b>%s</b>\n"+
					"Forecast: %s\n"+
					"Previous: %s",
				impactIcon(e.Impact), flag(e.Country), e.Title,
				wt, e.Actual, e.Forecast, e.Previous)

			if verdict != "" {
				msg += fmt.Sprintf("\n\n%s <b>%s</b>", vIcon, verdict)
			}

			if err := b.sendMessage("", msg); err != nil {
				log.Printf("[RESULT] %v", err)
			} else {
				log.Printf("[RESULT] %s %s = %s", e.Country, e.Title, e.Actual)
			}
			b.mu.Lock()
			state.ActualSent = true
			b.mu.Unlock()
		}
	}
}

// ── COMMAND HANDLER ──────────────────────────────────────────────────────────

func (b *Bot) handleCommand(chatID int64, text string) {
	cid := fmt.Sprintf("%d", chatID)
	b.mu.RLock()
	events := b.events
	b.mu.RUnlock()

	switch {
	case text == "/start" || text == "/help":
		b.sendWithKB(cid, fmtStart(), kbMain())

	case text == "/calendar" || text == "/today":
		msg := fmtToday(events)
		if len(msg) > 4000 {
			for _, c := range splitMessage(msg, 4000) {
				b.sendMessage(cid, c)
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			b.sendWithKB(cid, msg, kbCalendar())
		}

	case text == "/week":
		msg := fmtWeek(events)
		if len(msg) > 4000 {
			chunks := splitMessage(msg, 4000)
			for i, c := range chunks {
				if i == len(chunks)-1 {
					b.sendWithKB(cid, c, kbWeek())
				} else {
					b.sendMessage(cid, c)
				}
				time.Sleep(100 * time.Millisecond)
			}
		} else {
			b.sendWithKB(cid, msg, kbWeek())
		}

	case text == "/high":
		b.sendWithKB(cid, fmtHigh(events), kbHigh())

	case text == "/next":
		b.sendWithKB(cid, fmtNext(events, 10), kbNext())

	case text == "/alerts":
		b.sendWithKB(cid, b.fmtAlerts(), kbAlerts(b.alertsOn))

	case text == "/refresh":
		if err := b.fetchEvents(); err != nil {
			b.sendMessage(cid, fmt.Sprintf("\u274C Refresh failed: %v", err))
		} else {
			b.sendWithKB(cid, fmt.Sprintf("\u2705 Refreshed: %d events", len(b.events)), kbBack())
		}

	case text == "/chatid":
		b.sendWithKB(cid, fmt.Sprintf("\U0001F194 Chat ID: <code>%d</code>", chatID), kbBack())
	}
}

// ── CALLBACK HANDLER ─────────────────────────────────────────────────────────

func (b *Bot) handleCallback(cb *TGCallback) {
	if cb.Message == nil {
		b.answerCB(cb.ID, "")
		return
	}
	cid := fmt.Sprintf("%d", cb.Message.Chat.ID)
	mid := cb.Message.MessageID

	b.mu.RLock()
	events := b.events
	b.mu.RUnlock()

	var text string
	var kb *InlineKeyboardMarkup

	switch cb.Data {
	case "start":
		text = fmtStart()
		k := kbMain(); kb = &k
	case "calendar":
		text = fmtToday(events)
		k := kbCalendar(); kb = &k
	case "week":
		text = fmtWeek(events)
		k := kbWeek(); kb = &k
	case "high":
		text = fmtHigh(events)
		k := kbHigh(); kb = &k
	case "next":
		text = fmtNext(events, 10)
		k := kbNext(); kb = &k
	case "alerts":
		text = b.fmtAlerts()
		k := kbAlerts(b.alertsOn); kb = &k
	case "alerts_on":
		b.alertsOn = true
		text = b.fmtAlerts()
		k := kbAlerts(true); kb = &k
		b.answerCB(cb.ID, "\u2705 Alerts ON")
		b.editMsg(cid, mid, text, kb)
		return
	case "alerts_off":
		b.alertsOn = false
		text = b.fmtAlerts()
		k := kbAlerts(false); kb = &k
		b.answerCB(cb.ID, "\u274C Alerts OFF")
		b.editMsg(cid, mid, text, kb)
		return
	case "refresh":
		if err := b.fetchEvents(); err != nil {
			b.answerCB(cb.ID, "Refresh failed")
			return
		}
		b.answerCB(cb.ID, fmt.Sprintf("\u2705 %d events", len(b.events)))
		b.mu.RLock()
		newEv := b.events
		b.mu.RUnlock()
		text = fmtToday(newEv)
		k := kbCalendar(); kb = &k
		b.editMsg(cid, mid, text, kb)
		return
	case "chatid":
		text = fmt.Sprintf("\U0001F194 Chat ID: <code>%d</code>", cb.Message.Chat.ID)
		k := kbBack(); kb = &k
	default:
		b.answerCB(cb.ID, "")
		return
	}

	// Long messages: send new instead of edit
	if len(text) > 4000 {
		b.answerCB(cb.ID, "")
		chunks := splitMessage(text, 4000)
		for i, c := range chunks {
			if i == len(chunks)-1 && kb != nil {
				b.sendWithKB(cid, c, *kb)
			} else {
				b.sendMessage(cid, c)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return
	}

	b.answerCB(cb.ID, "")
	b.editMsg(cid, mid, text, kb)
}

// ── MAIN LOOP ────────────────────────────────────────────────────────────────

func (b *Bot) run() {
	log.Println("[BOT] Starting FF Calendar Bot...")

	if err := b.fetchEvents(); err != nil {
		log.Printf("[BOT] Initial fetch: %v", err)
	}

	// Smart refresh: 2min near events, 5min idle
	go func() {
		for {
			interval := 5 * time.Minute
			b.mu.RLock()
			for _, e := range b.events {
				t, err := parseEventTime(e.Date)
				if err != nil {
					continue
				}
				diff := time.Until(t)
				if diff > -30*time.Minute && diff < 45*time.Minute {
					interval = 2 * time.Minute
					break
				}
			}
			b.mu.RUnlock()
			time.Sleep(interval)
			if err := b.fetchEvents(); err != nil {
				log.Printf("[FETCH] %v", err)
			}
		}
	}()

	// Alert check every 30s
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			b.checkAlerts()
		}
	}()

	// Poll commands + callbacks
	log.Println("[BOT] Polling...")
	for {
		updates, err := b.getUpdates()
		if err != nil {
			log.Printf("[POLL] %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				go b.handleCallback(u.CallbackQuery)
				continue
			}
			if u.Message != nil && strings.HasPrefix(u.Message.Text, "/") {
				cmd := u.Message.Text
				if idx := strings.Index(cmd, "@"); idx != -1 {
					cmd = cmd[:idx]
				}
				go b.handleCommand(u.Message.Chat.ID, cmd)
			}
		}
	}
}

// ── HELPERS ──────────────────────────────────────────────────────────────────

func parseNumber(s string) float64 {
	s = strings.TrimSpace(s)
	for _, r := range []string{"%", "K", "M", "B", "T", ","} {
		s = strings.Replace(s, r, "", -1)
	}
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

func splitMessage(text string, maxLen int) []string {
	var chunks []string
	lines := strings.Split(text, "\n")
	cur := ""
	for _, line := range lines {
		if len(cur)+len(line)+1 > maxLen {
			if cur != "" {
				chunks = append(chunks, cur)
			}
			cur = line
		} else {
			if cur != "" {
				cur += "\n"
			}
			cur += line
		}
	}
	if cur != "" {
		chunks = append(chunks, cur)
	}
	return chunks
}

// ── ENTRYPOINT ───────────────────────────────────────────────────────────────

func main() {
	var err error
	WIB, err = time.LoadLocation("Asia/Jakarta")
	if err != nil {
		WIB = time.FixedZone("WIB", 7*60*60)
	}
	if BOT_TOKEN == "" {
		log.Fatal("[FATAL] BOT_TOKEN required")
	}
	if CHAT_ID == "" {
		log.Println("[WARN] CHAT_ID not set \u2014 use /chatid to get it")
	}
	bot := NewBot(BOT_TOKEN, CHAT_ID)
	bot.run()
}