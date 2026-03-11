package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arkcode369/ff-calendar-bot/internal/ports"
)

// ---------------------------------------------------------------------------
// Telegram Bot API types
// ---------------------------------------------------------------------------

// Update represents an incoming Telegram update.
type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

// Message represents a Telegram message.
type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from,omitempty"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}

// User represents a Telegram user.
type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

// CallbackQuery represents a callback from an inline keyboard button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data"`
}

// apiResponse wraps the Telegram Bot API response format.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	Description string          `json:"description,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
}

// sentMessage is the minimal response from sendMessage.
type sentMessage struct {
	MessageID int `json:"message_id"`
}

// ---------------------------------------------------------------------------
// Bot — Core Telegram bot engine
// ---------------------------------------------------------------------------

// CommandHandler handles a specific bot command.
type CommandHandler func(ctx context.Context, chatID string, userID int64, args string) error

// CallbackHandler handles an inline keyboard callback.
type CallbackHandler func(ctx context.Context, chatID string, msgID int, userID int64, data string) error

// Bot is the Telegram bot engine that handles polling, message routing,
// and implements ports.Messenger for outbound message delivery.
type Bot struct {
	token      string
	defaultID  string // default chat ID
	apiBase    string
	httpClient *http.Client

	// Command routing
	commands  map[string]CommandHandler
	callbacks map[string]CallbackHandler // prefix-based routing

	// Polling state
	offset int
	mu     sync.Mutex

	// Rate limiting: Telegram allows ~30 msg/sec to same chat
	sendMu   sync.Mutex
	lastSend time.Time
}

// NewBot creates a new Telegram bot with the given token and default chat ID.
func NewBot(token, defaultChatID string) *Bot {
	return &Bot{
		token:     token,
		defaultID: defaultChatID,
		apiBase:   fmt.Sprintf("https://api.telegram.org/bot%s", token),
		httpClient: &http.Client{
			Timeout: 60 * time.Second, // long-polling timeout + buffer
		},
		commands:  make(map[string]CommandHandler),
		callbacks: make(map[string]CallbackHandler),
	}
}

// compile-time interface check
var _ ports.Messenger = (*Bot)(nil)

// ---------------------------------------------------------------------------
// Command & Callback Registration
// ---------------------------------------------------------------------------

// RegisterCommand registers a handler for a slash command (e.g., "/today").
func (b *Bot) RegisterCommand(cmd string, handler CommandHandler) {
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	b.commands[cmd] = handler
	log.Printf("[BOT] Registered command: %s", cmd)
}

// RegisterCallback registers a handler for callbacks matching a prefix.
// E.g., RegisterCallback("cot:", handler) matches "cot:USD", "cot:EUR", etc.
func (b *Bot) RegisterCallback(prefix string, handler CallbackHandler) {
	b.callbacks[prefix] = handler
	log.Printf("[BOT] Registered callback prefix: %s", prefix)
}

// ---------------------------------------------------------------------------
// Long-Polling Loop
// ---------------------------------------------------------------------------

// StartPolling begins the long-polling loop. Blocks until ctx is cancelled.
func (b *Bot) StartPolling(ctx context.Context) error {
	log.Printf("[BOT] Starting long-polling loop")

	for {
		select {
		case <-ctx.Done():
			log.Printf("[BOT] Polling stopped: %v", ctx.Err())
			return ctx.Err()
		default:
		}

		updates, err := b.getUpdates(ctx, b.offset, 100, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("[BOT] getUpdates error: %v, retrying in 5s", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			b.offset = update.UpdateID + 1
			go b.handleUpdate(ctx, update)
		}
	}
}

// getUpdates calls the Telegram getUpdates API with long polling.
func (b *Bot) getUpdates(ctx context.Context, offset, limit, timeout int) ([]Update, error) {
	params := map[string]interface{}{
		"offset":  offset,
		"limit":   limit,
		"timeout": timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}

	var updates []Update
	if err := b.apiCall(ctx, "getUpdates", params, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// handleUpdate routes an incoming update to the appropriate handler.
func (b *Bot) handleUpdate(ctx context.Context, update Update) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[BOT] PANIC in handler: %v", r)
		}
	}()

	if update.CallbackQuery != nil {
		b.handleCallback(ctx, update.CallbackQuery)
		return
	}

	if update.Message != nil && update.Message.Text != "" {
		b.handleMessage(ctx, update.Message)
		return
	}
}

// handleMessage routes text messages to command handlers.
func (b *Bot) handleMessage(ctx context.Context, msg *Message) {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return // Ignore non-command messages
	}

	// Parse command and arguments: "/today USD" -> cmd="/today", args="USD"
	parts := strings.SplitN(text, " ", 2)
	cmd := strings.ToLower(parts[0])

	// Strip @BotUsername suffix for group commands: "/today@FFCalendarBot" -> "/today"
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}

	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	userID := int64(0)
	if msg.From != nil {
		userID = msg.From.ID
	}

	handler, ok := b.commands[cmd]
	if !ok {
		log.Printf("[BOT] Unknown command: %s from user %d", cmd, userID)
		_, _ = b.SendHTML(ctx, chatID, fmt.Sprintf(
			"Unknown command <code>%s</code>\nType /help for available commands.",
			cmd,
		))
		return
	}

	log.Printf("[BOT] Command %s from user %d in chat %s", cmd, userID, chatID)
	if err := handler(ctx, chatID, userID, args); err != nil {
		log.Printf("[BOT] Handler error for %s: %v", cmd, err)
		_, _ = b.SendHTML(ctx, chatID,
			fmt.Sprintf("Error processing <code>%s</code>: %s", cmd, err.Error()))
	}
}

// handleCallback routes inline keyboard callbacks.
func (b *Bot) handleCallback(ctx context.Context, cb *CallbackQuery) {
	chatID := ""
	msgID := 0
	if cb.Message != nil {
		chatID = strconv.FormatInt(cb.Message.Chat.ID, 10)
		msgID = cb.Message.MessageID
	}

	userID := cb.From.ID
	log.Printf("[BOT] Callback %q from user %d", cb.Data, userID)

	// Find handler by prefix match
	for prefix, handler := range b.callbacks {
		if strings.HasPrefix(cb.Data, prefix) {
			if err := handler(ctx, chatID, msgID, userID, cb.Data); err != nil {
				log.Printf("[BOT] Callback handler error for %q: %v", cb.Data, err)
				_ = b.AnswerCallback(ctx, cb.ID, "Error processing request")
				return
			}
			_ = b.AnswerCallback(ctx, cb.ID, "")
			return
		}
	}

	log.Printf("[BOT] Unhandled callback: %q", cb.Data)
	_ = b.AnswerCallback(ctx, cb.ID, "Unknown action")
}

// ---------------------------------------------------------------------------
// ports.Messenger Implementation — Outbound messaging
// ---------------------------------------------------------------------------

// SendMessage sends a plain text message.
func (b *Bot) SendMessage(ctx context.Context, chatID string, text string) (int, error) {
	if chatID == "" {
		chatID = b.defaultID
	}

	b.rateLimit()

	params := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}

	var msg sentMessage
	if err := b.apiCall(ctx, "sendMessage", params, &msg); err != nil {
		return 0, fmt.Errorf("sendMessage: %w", err)
	}
	return msg.MessageID, nil
}

// SendHTML sends an HTML-formatted message with link preview disabled.
func (b *Bot) SendHTML(ctx context.Context, chatID string, html string) (int, error) {
	if chatID == "" {
		chatID = b.defaultID
	}

	b.rateLimit()

	// Split long messages (Telegram limit: 4096 chars)
	chunks := splitMessage(html, 4096)
	var lastMsgID int

	for _, chunk := range chunks {
		params := map[string]interface{}{
			"chat_id":                  chatID,
			"text":                     chunk,
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		}

		var msg sentMessage
		if err := b.apiCall(ctx, "sendMessage", params, &msg); err != nil {
			return lastMsgID, fmt.Errorf("sendHTML: %w", err)
		}
		lastMsgID = msg.MessageID
	}

	return lastMsgID, nil
}

// SendWithKeyboard sends a message with an inline keyboard.
func (b *Bot) SendWithKeyboard(ctx context.Context, chatID string, text string, kb ports.InlineKeyboard) (int, error) {
	if chatID == "" {
		chatID = b.defaultID
	}

	b.rateLimit()

	// Convert ports.InlineKeyboard to Telegram API format
	keyboard := b.buildInlineKeyboard(kb)

	params := map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             keyboard,
	}

	var msg sentMessage
	if err := b.apiCall(ctx, "sendMessage", params, &msg); err != nil {
		return 0, fmt.Errorf("sendWithKeyboard: %w", err)
	}
	return msg.MessageID, nil
}

// EditMessage edits an existing message's text.
func (b *Bot) EditMessage(ctx context.Context, chatID string, msgID int, text string) error {
	if chatID == "" {
		chatID = b.defaultID
	}

	params := map[string]interface{}{
		"chat_id":                  chatID,
		"message_id":               msgID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}

	return b.apiCallNoResult(ctx, "editMessageText", params)
}

// EditWithKeyboard edits message text and keyboard.
func (b *Bot) EditWithKeyboard(ctx context.Context, chatID string, msgID int, text string, kb ports.InlineKeyboard) error {
	if chatID == "" {
		chatID = b.defaultID
	}

	keyboard := b.buildInlineKeyboard(kb)

	params := map[string]interface{}{
		"chat_id":                  chatID,
		"message_id":               msgID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
		"reply_markup":             keyboard,
	}

	return b.apiCallNoResult(ctx, "editMessageText", params)
}

// AnswerCallback acknowledges a callback query.
func (b *Bot) AnswerCallback(ctx context.Context, callbackID string, text string) error {
	params := map[string]interface{}{
		"callback_query_id": callbackID,
	}
	if text != "" {
		params["text"] = text
	}

	return b.apiCallNoResult(ctx, "answerCallbackQuery", params)
}

// DeleteMessage deletes a message.
func (b *Bot) DeleteMessage(ctx context.Context, chatID string, msgID int) error {
	if chatID == "" {
		chatID = b.defaultID
	}

	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": msgID,
	}

	return b.apiCallNoResult(ctx, "deleteMessage", params)
}

// ---------------------------------------------------------------------------
// Proactive messaging (for alerts and scheduled reports)
// ---------------------------------------------------------------------------

// Broadcast sends an HTML message to the default chat.
// Used by the alerter and scheduled report services.
func (b *Bot) Broadcast(ctx context.Context, html string) (int, error) {
	return b.SendHTML(ctx, b.defaultID, html)
}

// BroadcastWithKeyboard sends a message with keyboard to the default chat.
func (b *Bot) BroadcastWithKeyboard(ctx context.Context, html string, kb ports.InlineKeyboard) (int, error) {
	return b.SendWithKeyboard(ctx, b.defaultID, html, kb)
}

// ---------------------------------------------------------------------------
// Telegram API helpers
// ---------------------------------------------------------------------------

// apiCall makes a Telegram Bot API call and unmarshals the result.
func (b *Bot) apiCall(ctx context.Context, method string, params map[string]interface{}, result interface{}) error {
	url := fmt.Sprintf("%s/%s", b.apiBase, method)

	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("api call %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if !apiResp.OK {
		// Handle rate limiting (429)
		if apiResp.ErrorCode == 429 {
			log.Printf("[BOT] Rate limited on %s, waiting 5s", method)
			time.Sleep(5 * time.Second)
			return b.apiCall(ctx, method, params, result) // retry once
		}
		return fmt.Errorf("api error %d: %s", apiResp.ErrorCode, apiResp.Description)
	}

	if result != nil && len(apiResp.Result) > 0 {
		if err := json.Unmarshal(apiResp.Result, result); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}
	}

	return nil
}

// apiCallNoResult makes a Telegram API call without parsing the result.
func (b *Bot) apiCallNoResult(ctx context.Context, method string, params map[string]interface{}) error {
	return b.apiCall(ctx, method, params, nil)
}

// buildInlineKeyboard converts ports.InlineKeyboard to Telegram API format.
func (b *Bot) buildInlineKeyboard(kb ports.InlineKeyboard) map[string]interface{} {
	rows := make([][]map[string]string, 0, len(kb.Rows))
	for _, row := range kb.Rows {
		btnRow := make([]map[string]string, 0, len(row))
		for _, btn := range row {
			btnMap := map[string]string{"text": btn.Text}
			if btn.CallbackData != "" {
				btnMap["callback_data"] = btn.CallbackData
			}
			if btn.URL != "" {
				btnMap["url"] = btn.URL
			}
			btnRow = append(btnRow, btnMap)
		}
		rows = append(rows, btnRow)
	}
	return map[string]interface{}{"inline_keyboard": rows}
}

// rateLimit ensures we don't exceed Telegram's rate limits.
// Min 35ms between messages to same chat (~28 msg/sec, under 30 limit).
func (b *Bot) rateLimit() {
	b.sendMu.Lock()
	defer b.sendMu.Unlock()

	sinceLastSend := time.Since(b.lastSend)
	if sinceLastSend < 35*time.Millisecond {
		time.Sleep(35*time.Millisecond - sinceLastSend)
	}
	b.lastSend = time.Now()
}

// splitMessage splits a long message into chunks that fit Telegram's 4096 char limit.
// Splits on newlines when possible to preserve formatting.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the best split point (last newline before maxLen)
		splitAt := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > maxLen/2 {
			splitAt = idx + 1 // Include the newline in the first chunk
		}

		chunks = append(chunks, text[:splitAt])
		text = text[splitAt:]
	}

	return chunks
}

// DefaultChatID returns the configured default chat ID.
func (b *Bot) DefaultChatID() string {
	return b.defaultID
}
