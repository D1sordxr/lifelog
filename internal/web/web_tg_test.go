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

func init() { store.PINIterations = 1000 }

type tgEnv struct {
	t     *testing.T
	st    *store.Store
	now   time.Time
	allow tg.Allowlist
	srv   *httptest.Server
	opens int // счётчик «открытий» мини-приложения: у каждого свой query_id и hash
}

func newTGEnv(t *testing.T) *tgEnv {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), tz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &tgEnv{t: t, st: st, now: time.Date(2026, 11, 18, 7, 30, 0, 0, tz)}
	st.SetClock(func() time.Time { return e.now })
	e.allow = tg.ParseAllowlist([]string{"111", "@tester"})
	e.serve(e.allow)
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
		r.Header.Set("Cookie", cookie)
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
	e.opens++
	return tg.SignInitData(botToken, map[string]string{
		"query_id":  "Q" + strconv.Itoa(e.opens),
		"auth_date": strconv.FormatInt(e.now.Unix(), 10),
		"user":      `{"id":` + strconv.FormatInt(id, 10) + `,"first_name":"T","username":"` + username + `"}`,
	})
}

func cookieOf(resp *http.Response, name string) string {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

// signIn — вход через Telegram, возвращает значение cookie сессии.
func (e *tgEnv) signIn(id int64, username string) string {
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

const testPIN = "135790"

// login — вход + PIN (задаёт при первом входе). Возвращает заголовок Cookie.
func (e *tgEnv) login(id int64, username string) string {
	e.t.Helper()
	sess := sessionCookie + "=" + e.signIn(id, username)
	resp, body := e.req("POST", "/pin", sess, url.Values{"pin": {testPIN}, "pin2": {testPIN}}, nil)
	if resp.StatusCode != 200 {
		e.t.Fatalf("pin for %d: %d %s", id, resp.StatusCode, body)
	}
	return sess + "; " + unlockCookie + "=" + cookieOf(resp, unlockCookie)
}

func TestTelegramAuth(t *testing.T) {
	e := newTGEnv(t)

	// Telegram Web встраивает приложение во фрейм — это должно быть разрешено только ему.
	if resp, _ := e.req("GET", "/", "", nil, nil); resp.Header.Get("X-Frame-Options") != "" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'self' https://web.telegram.org") {
		t.Errorf("frame headers: %v", resp.Header)
	}

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
	if resp, _ := e.req("GET", "/", sessionCookie+"=1.9999999999.AAAA", nil, nil); resp.StatusCode != 200 {
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
	ownerSession := strings.TrimPrefix(strings.Split(owner, ";")[0], sessionCookie+"=")
	if resp, _ := e.req("GET", "/export.md?dl="+url.QueryEscape(ownerSession), "", nil, nil); resp.StatusCode != 401 {
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
	owner = e.login(111, "") // прошло 10 минут — разблокировка истекла, вводим PIN
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

func TestPINGate(t *testing.T) {
	e := newTGEnv(t)
	sess := sessionCookie + "=" + e.signIn(111, "")

	// PIN не задан: вместо приложения — экран установки PIN, API закрыт.
	if _, body := e.req("GET", "/history", sess, nil, nil); !strings.Contains(body, "Придумай PIN") || !strings.Contains(body, "pin.js") {
		t.Fatal("setup screen expected")
	}
	if resp, body := e.req("POST", "/wake", sess, url.Values{}, nil); resp.StatusCode != 401 || !strings.Contains(body, `"locked":true`) {
		t.Fatalf("API before PIN: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.req("POST", "/auth/download", sess, url.Values{}, nil); resp.StatusCode != 401 {
		t.Fatal("download token without PIN")
	}
	for _, bad := range []url.Values{{"pin": {"12345"}, "pin2": {"12345"}}, {"pin": {"123456"}, "pin2": {"654321"}}} {
		if resp, _ := e.req("POST", "/pin", sess, bad, nil); resp.StatusCode != 400 {
			t.Errorf("bad setup %v: %d", bad, resp.StatusCode)
		}
	}
	resp, _ := e.req("POST", "/pin", sess, url.Values{"pin": {testPIN}, "pin2": {testPIN}}, nil)
	unlocked := sess + "; " + unlockCookie + "=" + cookieOf(resp, unlockCookie)
	if _, body := e.req("GET", "/", unlocked, nil, nil); !strings.Contains(body, "Встал сейчас") || !strings.Contains(body, "Заблокировать") {
		t.Fatal("unlocked app expected")
	}

	// Перезаписать PIN через «установку» нельзя (даже из разблокированной сессии).
	if resp, _ := e.req("POST", "/pin", unlocked, url.Values{"pin": {"000000"}, "pin2": {"000000"}}, nil); resp.StatusCode != 401 {
		t.Fatalf("overwrite attempt must be a wrong-PIN try: %d", resp.StatusCode)
	}

	// Без cookie разблокировки — экран ввода PIN.
	if _, body := e.req("GET", "/", sess, nil, nil); !strings.Contains(body, "Введи PIN") {
		t.Fatal("unlock screen expected")
	}
	// Поддельная cookie разблокировки и чужая (другого пользователя) не принимаются.
	other := e.login(222, "tester")
	otherUnlock := strings.Split(other, "; ")[1]
	for _, c := range []string{sess + "; " + unlockCookie + "=1.9999999999.AAAA", sess + "; " + otherUnlock} {
		if _, body := e.req("GET", "/", c, nil, nil); !strings.Contains(body, "Введи PIN") {
			t.Errorf("foreign/forged unlock accepted: %s", c)
		}
	}
	// Сессионная cookie не годится как cookie разблокировки (другой тип токена).
	if _, body := e.req("GET", "/", sess+"; "+unlockCookie+"="+strings.TrimPrefix(sess, sessionCookie+"="), nil, nil); !strings.Contains(body, "Введи PIN") {
		t.Error("session token accepted as unlock token")
	}

	// Неверный PIN 5 раз → блокировка, и верный PIN не проходит, даже с новой сессией.
	// Одна ошибка уже была (попытка перезаписи), поэтому блокировку включает 4-я здесь.
	for i := 1; i <= 4; i++ {
		resp, _ := e.req("POST", "/pin", sess, url.Values{"pin": {"000000"}}, nil)
		if want := map[bool]int{true: 429, false: 401}[i == 4]; resp.StatusCode != want {
			t.Fatalf("wrong #%d: %d, want %d", i, resp.StatusCode, want)
		}
	}
	fresh := sessionCookie + "=" + e.signIn(111, "")
	resp, body := e.req("POST", "/pin", fresh, url.Values{"pin": {testPIN}}, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || !strings.Contains(body, "Слишком много попыток") {
		t.Fatalf("lockout: %d %s", resp.StatusCode, body)
	}
	e.now = e.now.Add(2 * time.Minute)
	if resp, _ := e.req("POST", "/pin", fresh, url.Values{"pin": {testPIN}}, nil); resp.StatusCode != 200 {
		t.Fatalf("after lockout: %d", resp.StatusCode)
	}

	// Разблокировка скользящая: запросы продлевают, простой 10 минут — блокирует.
	unlocked = e.login(111, "")
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(8 * time.Minute)
		resp, body := e.req("GET", "/", unlocked, nil, nil)
		if !strings.Contains(body, "Встал сейчас") {
			t.Fatalf("activity %d should keep unlocked", i)
		}
		unlocked = sess + "; " + unlockCookie + "=" + cookieOf(resp, unlockCookie)
	}
	e.now = e.now.Add(11 * time.Minute)
	if _, body := e.req("GET", "/", unlocked, nil, nil); !strings.Contains(body, "Введи PIN") {
		t.Fatal("idle unlock must expire")
	}

	// «Заблокировать» стирает разблокировку.
	unlocked = e.login(111, "")
	resp, _ = e.req("POST", "/pin/lock", unlocked, url.Values{}, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("lock: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == unlockCookie && c.MaxAge >= 0 {
			t.Fatal("lock must delete unlock cookie")
		}
	}
}

func TestPINChange(t *testing.T) {
	e := newTGEnv(t)
	c := e.login(111, "")
	sess := strings.Split(c, "; ")[0]

	// Без разблокировки — ни экрана смены, ни смены.
	if _, body := e.req("GET", "/pin/change", sess, nil, nil); !strings.Contains(body, "Введи PIN") {
		t.Fatal("change page must require unlock")
	}
	if resp, _ := e.req("POST", "/pin/change", sess, url.Values{"old": {testPIN}, "pin": {"111111"}, "pin2": {"111111"}}, nil); resp.StatusCode != 401 {
		t.Fatalf("change without unlock: %d", resp.StatusCode)
	}

	if _, body := e.req("GET", "/data", c, nil, nil); !strings.Contains(body, `href="/pin/change"`) {
		t.Fatal("data page must link to PIN change")
	}
	if _, body := e.req("GET", "/pin/change", c, nil, nil); !strings.Contains(body, `data-mode="change"`) || !strings.Contains(body, "Текущий PIN") {
		t.Fatal("change screen")
	}
	if resp, _ := e.req("POST", "/pin/change", c, url.Values{"old": {testPIN}, "pin": {"111111"}, "pin2": {"222222"}}, nil); resp.StatusCode != 400 {
		t.Fatalf("mismatch: %d", resp.StatusCode)
	}
	resp, body := e.req("POST", "/pin/change", c, url.Values{"old": {"000000"}, "pin": {"111111"}, "pin2": {"111111"}}, nil)
	if resp.StatusCode != 401 || !strings.Contains(body, "осталось попыток: 4") {
		t.Fatalf("wrong old: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.req("POST", "/pin/change", c, url.Values{"old": {testPIN}, "pin": {"111111"}, "pin2": {"111111"}}, nil); resp.StatusCode != 200 {
		t.Fatalf("change: %d", resp.StatusCode)
	}
	// Старый PIN больше не открывает, новый — открывает.
	if resp, _ := e.req("POST", "/pin", sess, url.Values{"pin": {testPIN}}, nil); resp.StatusCode != 401 {
		t.Fatalf("old PIN after change: %d", resp.StatusCode)
	}
	if resp, _ := e.req("POST", "/pin", sess, url.Values{"pin": {"111111"}}, nil); resp.StatusCode != 200 {
		t.Fatalf("new PIN: %d", resp.StatusCode)
	}
}

func TestInitDataReplay(t *testing.T) {
	e := newTGEnv(t)
	post := func(initData string) (int, string) {
		resp, body := e.req("POST", "/auth/telegram", "", url.Values{"init_data": {initData}}, nil)
		return resp.StatusCode, body
	}

	link := e.initData(111, "")
	if code, _ := post(link); code != 200 {
		t.Fatalf("first use: %d", code)
	}
	// Та же строка (скопированная ссылка #tgWebAppData=…) — отказ.
	if code, body := post(link); code != 401 || !strings.Contains(body, "уже использована") {
		t.Fatalf("replay: %d %s", code, body)
	}
	// И после «перезапуска» службы — отметка хранится в базе.
	e.serve(e.allow)
	if code, _ := post(link); code != 401 {
		t.Fatalf("replay after restart: %d", code)
	}
	// Новое открытие мини-приложения — новый initData, вход проходит.
	if code, _ := post(e.initData(111, "")); code != 200 {
		t.Fatalf("fresh open: %d", code)
	}
	// Старше 5 минут — отказ, даже ни разу не использованный.
	old := e.initData(111, "")
	e.now = e.now.Add(6 * time.Minute)
	if code, body := post(old); code != 401 || !strings.Contains(body, "устарела") {
		t.Fatalf("expired: %d %s", code, body)
	}
	// Отказ постороннему тоже «сжигает» строку.
	stranger := e.initData(999, "x")
	post(stranger)
	if code, _ := post(stranger); code != 401 {
		t.Fatalf("stranger replay: %d", code)
	}
}
