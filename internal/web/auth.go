package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"lifelog/internal/store"
	"lifelog/internal/tg"
)

// Вход через Telegram Mini App:
//  1. страница без сессии отдаёт auth.html; его JS отправляет Telegram.WebApp.initData
//     целиком на POST /auth/telegram;
//  2. сервер проверяет подпись initData токеном бота и свежесть auth_date (5 минут),
//     принимает каждый initData только один раз (ссылку #tgWebAppData=… нельзя
//     переиспользовать), сверяет пользователя с allowlist и ставит cookie сессии;
//  3. каждый запрос: cookie → пользователь → снова проверка по allowlist
//     (убрали из ALLOWED_USERS — доступ пропал после перезапуска).
// Данным из initDataUnsafe и любым id из запроса сервер не доверяет.
//  4. PIN (как экран блокировки): после входа нужен ещё и PIN — см. pin.go.

const (
	sessionCookie  = "lifelog_session"
	sessionTTL     = 30 * 24 * time.Hour
	initDataMaxAge = 5 * time.Minute
	downloadTTL    = 2 * time.Minute
	initDataLimit  = 8 << 10
)

type userKey struct{}
type sessionContextKey struct{}

type requestSession struct {
	token      string
	pinVersion int64
}

func sessionFrom(r *http.Request) requestSession {
	v, _ := r.Context().Value(sessionContextKey{}).(requestSession)
	return v
}

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
	if tok := r.URL.Query().Get("dl"); tok != "" && isExport(r.URL.Path) {
		binding, err := s.downloadBinding(r.URL.Path, r.URL.Query())
		if err == nil {
			if uid, err := s.verifyToken("download", tok, s.opt.Now(), binding); err == nil {
				if u := s.allowedUser(r.Context(), uid); u != nil {
					return r.WithContext(context.WithValue(r.Context(), userKey{}, u)), true
				}
			}
		}
	} else if u, token := s.sessionUser(r); u != nil {
		ctx := context.WithValue(r.Context(), userKey{}, u)
		ctx = context.WithValue(ctx, sessionContextKey{}, requestSession{token: token})
		return s.pinGate(w, r.WithContext(ctx), u)
	}
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/export.") {
		s.render(w, http.StatusOK, "auth.html", page{Title: "Вход", Bare: true, Login: true})
	} else {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "нужен вход через Telegram"})
	}
	return r, false
}

func (s *Server) allowedUser(ctx context.Context, uid int64) *store.User {
	u, err := s.st.UserByID(ctx, uid)
	if err != nil || !s.opt.Allow.Allowed(u.TGID, u.Username) {
		return nil
	}
	return u
}

func (s *Server) sessionUser(r *http.Request) (*store.User, string) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, ""
	}
	uid, err := s.verifyToken("session", c.Value, s.opt.Now(), "")
	if err != nil {
		return nil, ""
	}
	return s.allowedUser(r.Context(), uid), c.Value
}

// authTelegram — POST /auth/telegram, поле init_data = Telegram.WebApp.initData.
func (s *Server) authTelegram(w http.ResponseWriter, r *http.Request) {
	if !s.tgMode() {
		http.NotFound(w, r)
		return
	}
	if !readSmallForm(w, r, initDataLimit) {
		return
	}
	d, err := tg.ValidateInitData(r.PostForm.Get("init_data"), s.opt.BotToken, initDataMaxAge, s.opt.Now())
	if err != nil {
		log.Printf("auth: %v", err)
		msg := "Ссылка входа недействительна. Закрой и открой журнал заново из Telegram."
		if errors.Is(err, tg.ErrExpired) {
			msg = "Ссылка входа устарела. Закрой и открой журнал заново из Telegram."
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg})
		return
	}
	// Одноразовость: тот же initData (например, скопированная ссылка) второй раз не пускает.
	fresh, err := s.st.UseInitData(r.Context(), d.Hash, d.AuthDate.Add(initDataMaxAge+10*time.Minute))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !fresh {
		log.Printf("auth: initData replay for user %d", d.User.ID)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Эта ссылка входа уже использована. Открой журнал заново из Telegram."})
		return
	}
	tu := &d.User
	if !s.opt.Allow.Allowed(tu.ID, tu.Username) {
		log.Printf("auth: user %d not allowed", tu.ID)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": tg.NotAllowedText})
		return
	}
	if u, _ := s.sessionUser(r); u != nil && u.TGID == tu.ID {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": false})
		return
	}
	u, err := s.st.TelegramUser(r.Context(), tu.ID, tu.Username, s.opt.Allow.IsOwner(tu.ID))
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:        sessionCookie,
		Value:       s.signToken("session", u.ID, s.opt.Now().Add(sessionTTL), ""),
		Path:        "/",
		MaxAge:      int(sessionTTL / time.Second),
		HttpOnly:    true,
		Secure:      true,
		SameSite:    http.SameSiteNoneMode, // Telegram Web/Desktop открывают приложение во фрейме
		Partitioned: true,
	})
	clearUnlock(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": true})
}

// downloadToken — POST /auth/download: ссылка на один формат и диапазон.
func (s *Server) downloadToken(w http.ResponseWriter, r *http.Request) {
	u, ok := r.Context().Value(userKey{}).(*store.User)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "нужен вход"})
		return
	}
	if !readSmallForm(w, r, pinFormLimit) {
		return
	}
	binding, err := s.downloadBinding("/export."+r.PostForm.Get("format"), r.PostForm)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token := s.signToken("download", u.ID, s.opt.Now().Add(downloadTTL), binding)
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func isExport(path string) bool {
	switch path {
	case "/export.csv", "/export.json", "/export.md":
		return true
	}
	return false
}

func (s *Server) downloadBinding(path string, values url.Values) (string, error) {
	if !isExport(path) {
		return "", errors.New("неизвестный формат экспорта")
	}
	from, to, _, err := s.rangeParams(values)
	if err != nil {
		return "", err
	}
	return path + "|" + fmtTimeBound(from) + "|" + fmtTimeBound(to), nil
}

func fmtTimeBound(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// Случайная часть отличает даже сессии одного пользователя, выданные в одну секунду.
// Тип токена и привязка подписываются вместе с его полями.
func (s *Server) signToken(kind string, uid int64, exp time.Time, binding string) string {
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(exp.Unix(), 10) + "." + rand.Text()
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(kind, payload, binding))
}

func (s *Server) verifyToken(kind, tok string, now time.Time, binding string) (int64, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 || parts[2] == "" {
		return 0, errors.New("некорректный токен")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	payload := strings.Join(parts[:3], ".")
	if err != nil || !hmac.Equal(sig, s.mac(kind, payload, binding)) {
		return 0, errors.New("неверная подпись токена")
	}
	uid, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || now.Unix() >= exp || uid <= 0 {
		return 0, errors.New("токен недействителен")
	}
	return uid, nil
}

func (s *Server) mac(kind, payload, binding string) []byte {
	m := hmac.New(sha256.New, s.sessKey)
	m.Write([]byte(kind + "|" + payload + "|" + binding))
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
