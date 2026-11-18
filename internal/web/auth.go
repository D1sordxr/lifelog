package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lifelog/internal/store"
	"lifelog/internal/tg"
)

// Вход через Telegram Mini App:
//  1. страница без сессии отдаёт auth.html; его JS отправляет Telegram.WebApp.initData
//     целиком на POST /auth/telegram;
//  2. сервер проверяет подпись initData токеном бота и свежесть auth_date,
//     сверяет пользователя с allowlist и ставит подписанную cookie сессии;
//  3. каждый запрос: cookie → пользователь → снова проверка по allowlist
//     (убрали из ALLOWED_USERS — доступ пропал после перезапуска).
// Данным из initDataUnsafe и любым id из запроса сервер не доверяет.
//  4. PIN (как экран блокировки): после входа нужен ещё и PIN — см. pin.go.

const (
	sessionCookie  = "lifelog_session"
	sessionTTL     = 30 * 24 * time.Hour
	initDataMaxAge = time.Hour
	downloadTTL    = 5 * time.Minute
)

type userKey struct{}

func (s *Server) tgMode() bool { return s.opt.BotToken != "" }

// days — дни текущего пользователя.
func (s *Server) days(r *http.Request) *store.Days {
	if u, ok := r.Context().Value(userKey{}).(*store.User); ok {
		return s.st.For(u.ID)
	}
	if s.tgMode() {
		return s.st.For(0) // сюда не попасть без входа; на всякий случай — пустой пользователь
	}
	return s.st.For(store.OwnerID)
}

func isPublic(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/healthz", p == "/manifest.json", p == "/favicon.ico",
		p == "/apple-touch-icon.png", p == "/apple-touch-icon-precomposed.png",
		strings.HasPrefix(p, "/static/"),
		r.Method == http.MethodPost && p == "/auth/telegram":
		return true
	}
	return false
}

// authenticate возвращает запрос с пользователем в контексте или false,
// если ответ (страница входа / 401) уже записан.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !s.tgMode() || isPublic(r) {
		return r, true
	}
	now := s.opt.Now()
	var uid int64
	if c, err := r.Cookie(sessionCookie); err == nil {
		uid, _ = s.verifyToken("session", c.Value, now)
	}
	// Ссылка на скачивание экспорта для Telegram.WebApp.downloadFile: клиент Telegram
	// качает файл сам, без cookie, поэтому в URL — короткоживущий подписанный токен.
	viaDownload := false
	if uid == 0 && strings.HasPrefix(r.URL.Path, "/export.") {
		uid, _ = s.verifyToken("download", r.URL.Query().Get("dl"), now)
		viaDownload = uid != 0 // токен выдаётся только разблокированной сессии
	}
	if uid != 0 {
		u, err := s.st.UserByID(r.Context(), uid)
		if err == nil && s.opt.Allow.Allowed(u.TGID, u.Username) {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
			if viaDownload {
				return r, true
			}
			return s.pinGate(w, r, u)
		}
	}
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/export.") {
		s.render(w, http.StatusOK, "auth.html", page{Title: "Вход", Bare: true, Script: "auth.js"})
	} else {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "нужен вход через Telegram"})
	}
	return r, false
}

// authTelegram — POST /auth/telegram, поле init_data = Telegram.WebApp.initData.
func (s *Server) authTelegram(w http.ResponseWriter, r *http.Request) {
	if !s.tgMode() {
		http.NotFound(w, r)
		return
	}
	tu, err := tg.ValidateInitData(r.PostFormValue("init_data"), s.opt.BotToken, initDataMaxAge, s.opt.Now())
	if err != nil {
		log.Printf("auth: %v", err)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	if !s.opt.Allow.Allowed(tu.ID, tu.Username) {
		log.Printf("auth: user %d not allowed", tu.ID)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": tg.NotAllowedText})
		return
	}
	u, err := s.st.TelegramUser(r.Context(), tu.ID, tu.Username, s.opt.Allow.IsOwner(tu.ID, tu.Username))
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:        sessionCookie,
		Value:       s.signToken("session", u.ID, s.opt.Now().Add(sessionTTL)),
		Path:        "/",
		MaxAge:      int(sessionTTL / time.Second),
		HttpOnly:    true,
		Secure:      true,
		SameSite:    http.SameSiteNoneMode, // Telegram Web/Desktop открывают приложение во фрейме
		Partitioned: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// downloadToken — POST /auth/download: токен для ссылки на экспорт (5 минут).
func (s *Server) downloadToken(w http.ResponseWriter, r *http.Request) {
	u, ok := r.Context().Value(userKey{}).(*store.User)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "нужен вход"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": s.signToken("download", u.ID, s.opt.Now().Add(downloadTTL))})
}

// signToken: "uid.exp.base64(HMAC(key, kind|uid.exp))". Ключ выводится из токена бота.
func (s *Server) signToken(kind string, uid int64, exp time.Time) string {
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(exp.Unix(), 10)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(kind, payload))
}

func (s *Server) verifyToken(kind, tok string, now time.Time) (int64, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("bad token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, s.mac(kind, parts[0]+"."+parts[1])) {
		return 0, fmt.Errorf("bad signature")
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() > exp {
		return 0, fmt.Errorf("expired")
	}
	return uid, nil
}

func (s *Server) mac(kind, payload string) []byte {
	m := hmac.New(sha256.New, s.sessKey)
	m.Write([]byte(kind + "|" + payload))
	return m.Sum(nil)
}

func sessionKey(botToken string) []byte {
	m := hmac.New(sha256.New, []byte("lifelog-session-v1"))
	m.Write([]byte(botToken))
	return m.Sum(nil)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
