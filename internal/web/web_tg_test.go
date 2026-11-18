package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lifelog/internal/store"
	"lifelog/internal/tg"
)

const botToken = "123456:TEST-token"

type tgEnv struct {
	t     *testing.T
	st    *store.Store
	now   time.Time
	allow tg.Allowlist
	srv   *httptest.Server
}

func newTGEnv(t *testing.T) *tgEnv {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), tz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &tgEnv{t: t, st: st, now: time.Date(2026, 11, 18, 7, 30, 0, 0, tz)}
	st.SetClock(func() time.Time { return e.now })
	e.serve(tg.ParseAllowlist([]string{"111", "@tester"}))
	return e
}

// serve (пере)запускает сервер с другим allowlist на той же базе — как перезапуск с новым env.
func (e *tgEnv) serve(allow tg.Allowlist) {
	if e.srv != nil {
		e.srv.Close()
	}
	e.srv = httptest.NewServer(New(e.st, Options{
		BotToken: botToken, Allow: allow, Now: func() time.Time { return e.now },
	}))
	e.t.Cleanup(e.srv.Close)
}

func (e *tgEnv) req(method, path, cookie string, form url.Values, hdr map[string]string) (*http.Response, string) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		r.Header.Set("Cookie", sessionCookie+"="+cookie)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *tgEnv) initData(id int64, username string) string {
	return tg.SignInitData(botToken, map[string]string{
		"auth_date": strconv.FormatInt(e.now.Unix(), 10),
		"user":      `{"id":` + strconv.FormatInt(id, 10) + `,"first_name":"T","username":"` + username + `"}`,
	})
}

// login возвращает значение cookie сессии.
func (e *tgEnv) login(id int64, username string) string {
	e.t.Helper()
	resp, body := e.req("POST", "/auth/telegram", "", url.Values{"init_data": {e.initData(id, username)}}, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("login %d: %d %s", id, resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || !c.Secure {
				e.t.Error("cookie must be HttpOnly and Secure")
			}
			return c.Value
		}
	}
	e.t.Fatal("no session cookie")
	return ""
}

func TestTelegramAuth(t *testing.T) {
	e := newTGEnv(t)

	// Без входа: страница входа, API закрыт, статика открыта.
	if resp, body := e.req("GET", "/", "", nil, nil); resp.StatusCode != 200 || !strings.Contains(body, "auth.js") ||
		!strings.Contains(body, "telegram-web-app.js") || strings.Contains(body, "Встал сейчас") {
		t.Fatalf("anonymous /: %d", resp.StatusCode)
	}
	for _, p := range []string{"/wake", "/days/1/field"} {
		if resp, _ := e.req("POST", p, "", url.Values{}, nil); resp.StatusCode != 401 {
			t.Errorf("anonymous POST %s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := e.req("GET", "/export.md", "", nil, nil); resp.StatusCode != 401 {
		t.Errorf("anonymous export: %d", resp.StatusCode)
	}
	if resp, _ := e.req("GET", "/static/style.css", "", nil, nil); resp.StatusCode != 200 {
		t.Errorf("static: %d", resp.StatusCode)
	}

	// Подделки и посторонние.
	forged := strings.Replace(e.initData(999, "x"), "999", "111", 1)
	if resp, _ := e.req("POST", "/auth/telegram", "", url.Values{"init_data": {forged}}, nil); resp.StatusCode != 401 {
		t.Errorf("forged initData: %d", resp.StatusCode)
	}
	resp, body := e.req("POST", "/auth/telegram", "", url.Values{"init_data": {e.initData(999, "stranger")}}, nil)
	if resp.StatusCode != 403 || !strings.Contains(body, tg.NotAllowedText) {
		t.Errorf("stranger: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.req("GET", "/", "1.9999999999.AAAA", nil, nil); resp.StatusCode != 200 {
		t.Errorf("forged cookie should show login page: %d", resp.StatusCode)
	}

	// Владелец (по id) и тестер (по username).
	owner := e.login(111, "")
	if resp, _ := e.req("POST", "/wake", owner, url.Values{}, nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner wake: %d", resp.StatusCode)
	}
	e.req("POST", "/days/1/field", owner, url.Values{"name": {"report"}, "value": {"секрет владельца"}}, nil)
	if _, body := e.req("GET", "/", owner, nil, nil); !strings.Contains(body, "Лёг сейчас") {
		t.Fatal("owner today")
	}

	tester := e.login(222, "Tester")
	if _, body := e.req("GET", "/", tester, nil, nil); !strings.Contains(body, "Встал сейчас") || strings.Contains(body, "Лёг сейчас") {
		t.Error("tester must have own empty journal")
	}
	if resp, _ := e.req("GET", "/days/1", tester, nil, nil); resp.StatusCode != 404 {
		t.Errorf("tester opens owner's day: %d", resp.StatusCode)
	}
	if resp, _ := e.req("POST", "/days/1/field", tester, url.Values{"name": {"report"}, "value": {"x"}}, nil); resp.StatusCode != 404 {
		t.Errorf("tester edits owner's day: %d", resp.StatusCode)
	}
	if _, body := e.req("GET", "/export.md", tester, nil, nil); strings.Contains(body, "секрет") {
		t.Error("tester export leaks owner's data")
	}

	// Ссылка на скачивание для Telegram.WebApp.downloadFile.
	_, body = e.req("POST", "/auth/download", owner, url.Values{}, nil)
	var j struct{ Token string }
	json.Unmarshal([]byte(body), &j)
	if resp, body := e.req("GET", "/export.md?dl="+url.QueryEscape(j.Token), "", nil, nil); resp.StatusCode != 200 || !strings.Contains(body, "секрет владельца") {
		t.Errorf("download link: %d", resp.StatusCode)
	}
	if _, body := e.req("GET", "/days/1?dl="+url.QueryEscape(j.Token), "", nil, nil); strings.Contains(body, "секрет") {
		t.Error("download token must work only for /export.*")
	}
	if resp, _ := e.req("GET", "/export.md?dl="+url.QueryEscape(owner), "", nil, nil); resp.StatusCode != 401 {
		t.Error("session cookie must not work as download token")
	}
	e.now = e.now.Add(10 * time.Minute)
	if resp, _ := e.req("GET", "/export.md?dl="+url.QueryEscape(j.Token), "", nil, nil); resp.StatusCode != 401 {
		t.Error("download token must expire")
	}

	// CSRF: POST с чужого сайта.
	if resp, _ := e.req("POST", "/wake", owner, url.Values{}, map[string]string{"Sec-Fetch-Site": "cross-site"}); resp.StatusCode != 403 {
		t.Errorf("cross-site POST: %d", resp.StatusCode)
	}

	// Тестера убрали из ALLOWED_USERS → доступ пропал, владелец работает.
	e.serve(tg.ParseAllowlist([]string{"111"}))
	if _, body := e.req("GET", "/", tester, nil, nil); !strings.Contains(body, "auth.js") {
		t.Error("revoked tester still has access")
	}
	if _, body := e.req("GET", "/", owner, nil, nil); !strings.Contains(body, "Лёг сейчас") {
		t.Error("owner lost access")
	}

	// Сессия истекает.
	e.now = e.now.Add(sessionTTL + time.Hour)
	if _, body := e.req("GET", "/", owner, nil, nil); !strings.Contains(body, "auth.js") {
		t.Error("expired session accepted")
	}
}
