package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// NotAllowedText — ответ всем, кого нет в allowlist.
const NotAllowedText = "You are not allowed to use this app"

// Bot — минимальный бот на long polling: ставит кнопку меню «Журнал»
// и отвечает на сообщения кнопкой, открывающей мини-приложение.
// Вебхук не нужен: приложение не обязано быть доступно из интернета.
type Bot struct {
	Token     string
	WebAppURL string
	Allow     Allowlist
	APIBase   string // по умолчанию https://api.telegram.org
	Client    *http.Client
	Logf      func(string, ...any)
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From *struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
	Text string `json:"text"`
}

type webApp struct {
	URL string `json:"url"`
}

// Run работает до отмены ctx.
func (b *Bot) Run(ctx context.Context) {
	if b.APIBase == "" {
		b.APIBase = "https://api.telegram.org"
	}
	if b.Client == nil {
		b.Client = &http.Client{Timeout: 70 * time.Second}
	}
	if b.Logf == nil {
		b.Logf = func(string, ...any) {}
	}
	if err := b.Setup(ctx); err != nil {
		b.Logf("telegram setup: %v", err)
	}
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		var ups []update
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": 50, "allowed_updates": []string{"message"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.Logf("telegram getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			if err := b.Handle(ctx, u.Message); err != nil {
				b.Logf("telegram reply: %v", err)
			}
		}
	}
}

// Setup: снять вебхук (иначе getUpdates не работает), поставить кнопку меню и команды.
func (b *Bot) Setup(ctx context.Context) error {
	if err := b.call(ctx, "deleteWebhook", map[string]any{}, nil); err != nil {
		return err
	}
	if err := b.call(ctx, "setChatMenuButton", map[string]any{
		"menu_button": map[string]any{"type": "web_app", "text": "Журнал", "web_app": webApp{b.WebAppURL}},
	}, nil); err != nil {
		return err
	}
	return b.call(ctx, "setMyCommands", map[string]any{
		"commands": []map[string]string{{"command": "start", "description": "Открыть журнал"}},
	}, nil)
}

// Handle отвечает на одно сообщение (только в личных чатах).
func (b *Bot) Handle(ctx context.Context, m *message) error {
	if m == nil || m.From == nil || m.Chat.Type != "private" {
		return nil
	}
	if !b.Allow.Allowed(m.From.ID, m.From.Username) {
		return b.send(ctx, m.Chat.ID, NotAllowedText, nil)
	}
	return b.send(ctx, m.Chat.ID, "Журнал дня. Открой кнопкой ниже или через «Журнал» в меню.", map[string]any{
		"inline_keyboard": [][]map[string]any{{{"text": "Открыть журнал", "web_app": webApp{b.WebAppURL}}}},
	})
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, markup any) error {
	req := map[string]any{"chat_id": chatID, "text": text}
	if markup != nil {
		req["reply_markup"] = markup
	}
	return b.call(ctx, "sendMessage", req, nil)
}

func (b *Bot) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.APIBase+"/bot"+b.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return b.clean(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.Client.Do(req)
	if err != nil {
		return b.clean(err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		return fmt.Errorf("%s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// clean убирает токен из текста ошибки (net/http включает URL в ошибки).
func (b *Bot) clean(err error) error {
	if b.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), b.Token, "***"))
}
