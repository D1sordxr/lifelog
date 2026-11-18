package web

import (
	"context"
	"encoding/json"
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

type authFixture struct {
	t      *testing.T
	server *Server
	st     *store.Store
	now    time.Time
	launch int
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &authFixture{t: t, st: st, now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	st.SetClock(func() time.Time { return f.now })
	f.server = New(st, Options{BotToken: botToken, Allow: tg.ParseAllowlist([]string{"111", "222"}), Now: func() time.Time { return f.now }})
	return f
}

func (f *authFixture) request(method, path, contentType, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", contentType)
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(c)
		}
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	return w
}

func (f *authFixture) post(path string, body url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return f.request(http.MethodPost, path, "application/x-www-form-urlencoded", body.Encode(), cookies...)
}

func (f *authFixture) initData(id int64) url.Values {
	f.launch++
	return url.Values{"init_data": {tg.SignInitData(botToken, map[string]string{
		"query_id":  strconv.Itoa(f.launch),
		"auth_date": strconv.FormatInt(f.now.Unix(), 10),
		"user":      `{"id":` + strconv.FormatInt(id, 10) + `}`,
	})}}
}

func (f *authFixture) cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	f.t.Helper()
	for _, line := range w.Header().Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err == nil && c.Name == name {
			return c
		}
	}
	f.t.Fatalf("ответ %d без ожидаемой cookie %s", w.Code, name)
	return nil
}

func (f *authFixture) signIn(id int64) *http.Cookie {
	f.t.Helper()
	return f.cookie(f.post("/auth/telegram", f.initData(id)), sessionCookie)
}

func (f *authFixture) unlock(session *http.Cookie, pin string) *http.Cookie {
	f.t.Helper()
	return f.cookie(f.post("/pin", url.Values{"pin": {pin}, "pin2": {pin}}, session), unlockCookie)
}

func (f *authFixture) wantPIN(mode string, cookies ...*http.Cookie) {
	f.t.Helper()
	w := f.request(http.MethodGet, "/history", "", "", cookies...)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `data-mode="`+mode+`"`) {
		f.t.Fatalf("ожидался PIN-экран %s, ответ %d", mode, w.Code)
	}
}

func TestFreshTelegramLaunchWithSession(t *testing.T) {
	f := newAuthFixture(t)
	session := f.signIn(111)
	unlock := f.unlock(session, testPIN)

	launch := f.initData(111)
	w := f.post("/auth/telegram", launch, session, unlock)
	if w.Code != http.StatusOK || w.Header().Get("Set-Cookie") != "" || !strings.Contains(w.Body.String(), `"changed":false`) {
		t.Fatalf("повторное открытие сменило сессию: %d %s", w.Code, w.Body)
	}
	if w := f.post("/auth/telegram", launch); w.Code != http.StatusUnauthorized {
		t.Fatalf("новый запуск не погашен: %d", w.Code)
	}
	w = f.request(http.MethodGet, "/history", "", "", session, unlock)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "История") || !strings.Contains(w.Body.String(), "auth.js") {
		t.Fatalf("журнал должен остаться разблокирован: %d", w.Code)
	}

	otherSession := f.signIn(222)
	f.unlock(otherSession, testPIN)
	w = f.post("/auth/telegram", f.initData(222), session, unlock)
	switched := f.cookie(w, sessionCookie)
	if w.Code != http.StatusOK || switched.Value == session.Value ||
		f.cookie(w, unlockCookie).MaxAge >= 0 || !strings.Contains(w.Body.String(), `"changed":true`) {
		t.Fatalf("переключение аккаунта: %d %s", w.Code, w.Body)
	}
	f.wantPIN("unlock", switched, unlock)
}

func TestAuthAndPINFormLimits(t *testing.T) {
	f := newAuthFixture(t)
	session := f.signIn(111)
	unlock := f.unlock(session, testPIN)
	for _, path := range []string{"/auth/telegram", "/pin", "/pin/change", "/pin/lock"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				contentType, body string
				status            int
			}{
				{"multipart/form-data; boundary=test", "", http.StatusUnsupportedMediaType},
				{"application/x-www-form-urlencoded", "a=%ZZ", http.StatusBadRequest},
				{"application/x-www-form-urlencoded", "a=" + strings.Repeat("a", initDataLimit), http.StatusRequestEntityTooLarge},
			} {
				if w := f.request(http.MethodPost, path, tc.contentType, tc.body, session, unlock); w.Code != tc.status {
					t.Errorf("%s: %d вместо %d", tc.contentType, w.Code, tc.status)
				}
			}
		})
	}
	if w := f.post("/pin", url.Values{"a": {strings.Repeat("a", pinFormLimit)}}, session); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("лимит формы PIN: %d", w.Code)
	}
	st, err := f.st.PINStatus(context.Background(), store.OwnerID)
	if err != nil || st.Failed != 0 {
		t.Fatalf("ошибки разбора засчитались как попытки: %+v %v", st, err)
	}
}

func TestUnlockBoundToSessionAndRevoked(t *testing.T) {
	f := newAuthFixture(t)
	a, b := f.signIn(111), f.signIn(111)
	if a.Value == b.Value {
		t.Fatal("два входа в одну секунду должны выдать разные сессии")
	}
	aUnlock, bUnlock := f.unlock(a, testPIN), f.unlock(b, testPIN)
	f.wantPIN("unlock", b, aUnlock)
	if w := f.post("/pin/lock", nil, a); w.Code != http.StatusUnauthorized {
		t.Fatalf("блокировка без разблокировки: %d", w.Code)
	}
	if w := f.post("/pin/lock", nil, a, aUnlock); w.Code != http.StatusSeeOther {
		t.Fatalf("блокировка: %d", w.Code)
	}
	f.wantPIN("unlock", a, aUnlock)
	f.wantPIN("unlock", b, bUnlock)

	aUnlock, bUnlock = f.unlock(a, testPIN), f.unlock(b, testPIN)
	newPIN := "654321"
	w := f.post("/pin/change", url.Values{"old": {testPIN}, "pin": {newPIN}, "pin2": {newPIN}}, a, aUnlock)
	if w.Code != http.StatusOK {
		t.Fatalf("смена PIN: %d %s", w.Code, w.Body)
	}
	newUnlock := f.cookie(w, unlockCookie)
	f.wantPIN("unlock", a, aUnlock)
	f.wantPIN("unlock", b, bUnlock)
	w = f.request(http.MethodGet, "/history", "", "", a, newUnlock)
	if !strings.Contains(w.Body.String(), "История") {
		t.Fatal("разблокировка после смены не работает")
	}
	if err := f.st.ResetPIN(context.Background(), 111); err != nil {
		t.Fatal(err)
	}
	f.wantPIN("setup", a, newUnlock)
	f.unlock(b, testPIN)
	f.wantPIN("unlock", a, newUnlock)
}

func TestDownloadBoundToExportAndExpires(t *testing.T) {
	f := newAuthFixture(t)
	session := f.signIn(111)
	unlock := f.unlock(session, testPIN)
	form := url.Values{"format": {"csv"}, "from": {"2026-11-18"}, "to": {"2026-11-19"}}
	w := f.post("/auth/download", form, session, unlock)
	var grant struct{ Token string }
	if err := json.Unmarshal(w.Body.Bytes(), &grant); err != nil || grant.Token == "" || w.Code != http.StatusOK {
		t.Fatalf("выдача токена: %d %v", w.Code, err)
	}
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/export.csv?to=2026-11-19&from=2026-11-18", http.StatusOK},
		{"/export.json?to=2026-11-19&from=2026-11-18", http.StatusUnauthorized},
		{"/export.csv?from=2026-11-18&to=2026-11-20", http.StatusUnauthorized},
		{"/export.csv?from=2026-11-17&to=2026-11-19", http.StatusUnauthorized},
		{"/export.csv?inline=1", http.StatusUnauthorized},
	} {
		if w := f.request(http.MethodGet, tc.path+"&dl="+url.QueryEscape(grant.Token), "", ""); w.Code != tc.want {
			t.Errorf("%s: %d вместо %d", tc.path, w.Code, tc.want)
		}
	}
	f.now = f.now.Add(2 * time.Minute)
	if w := f.request(http.MethodGet, "/export.csv?from=2026-11-18&to=2026-11-19&dl="+url.QueryEscape(grant.Token), "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("просроченный токен: %d", w.Code)
	}
}

func TestInternalErrorsAreNeutral(t *testing.T) {
	f := newAuthFixture(t)
	s := New(f.st, Options{})
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/history", ""},
		{http.MethodPost, "/days/1/field", "name=report&value=test"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "внутренняя ошибка") ||
			strings.Contains(w.Body.String(), "database is closed") {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
}
