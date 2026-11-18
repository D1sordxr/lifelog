package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lifelog/internal/store"
)

var tz = time.FixedZone("UTC+4", 4*60*60)

type env struct {
	t   *testing.T
	srv *httptest.Server
	now time.Time
	c   *http.Client
}

func newEnv(t *testing.T, opt Options) *env {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"), tz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, now: time.Date(2026, 11, 18, 7, 30, 0, 0, tz)}
	clock := func() time.Time { return e.now }
	st.SetClock(clock)
	opt.Now = clock
	e.srv = httptest.NewServer(New(st, opt))
	t.Cleanup(e.srv.Close)
	e.c = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) do(method, path string, form url.Values) (int, string) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestDayFlow(t *testing.T) {
	e := newEnv(t, Options{})

	if code, body := e.do("GET", "/", nil); code != 200 || !strings.Contains(body, "Встал сейчас") || strings.Contains(body, "Лёг сейчас") {
		t.Fatalf("empty today: %d", code)
	}
	if code, _ := e.do("POST", "/wake", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("wake: %d", code)
	}
	code, body := e.do("GET", "/", nil)
	if code != 200 || !strings.Contains(body, "Лёг сейчас") || !strings.Contains(body, `value="2026-11-18T07:30"`) {
		t.Fatalf("today after wake: %d\n%s", code, body)
	}

	// автосохранение
	e.now = e.now.Add(10 * time.Hour)
	code, body = e.do("POST", "/days/1/field", url.Values{"name": {"train_start"}, "value": {"now"}})
	var j map[string]any
	json.Unmarshal([]byte(body), &j)
	if code != 200 || j["value"] != "2026-11-18T17:30" {
		t.Fatalf("field now: %d %s", code, body)
	}
	if code, body = e.do("POST", "/days/1/field", url.Values{"name": {"energy"}, "value": {"7"}}); code != 400 || !strings.Contains(body, "от 1 до 5") {
		t.Fatalf("bad energy: %d %s", code, body)
	}
	e.do("POST", "/days/1/field", url.Values{"name": {"report"}, "value": {"Хороший день"}})

	// отбой после полуночи
	e.now = time.Date(2026, 11, 19, 0, 45, 0, 0, tz)
	if code, _ := e.do("POST", "/days/1/bed", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("bed: %d", code)
	}
	if _, body = e.do("GET", "/", nil); !strings.Contains(body, "Встал сейчас") || !strings.Contains(body, "00:45") {
		t.Fatalf("today after bed:\n%s", body)
	}

	// следующий день → сон 7:15
	e.now = time.Date(2026, 11, 19, 8, 0, 0, 0, tz)
	e.do("POST", "/wake", url.Values{})
	if _, body = e.do("GET", "/history", nil); !strings.Contains(body, "сон 7:15") || !strings.Contains(body, "Хороший день") {
		t.Fatalf("history:\n%s", body)
	}
	if code, body = e.do("GET", "/chart?d=30", nil); code != 200 || !strings.Contains(body, `class="bar"`) {
		t.Fatalf("chart: %d", code)
	}
	code, body = e.do("GET", "/export.md?from=2026-11-19&to=2026-11-19", nil)
	if code != 200 || !strings.Contains(body, "| 2026-11-19 Чт | 08:00 | — | 7:15 |") || strings.Contains(body, "2026-11-18 Ср") {
		t.Fatalf("export.md: %d\n%s", code, body)
	}
	if code, _ = e.do("GET", "/export.csv?from=bad", nil); code != 400 {
		t.Fatalf("bad range: %d", code)
	}
	for _, p := range []string{"/days/1", "/data", "/manifest.json", "/static/style.css", "/apple-touch-icon.png"} {
		if code, _ := e.do("GET", p, nil); code != 200 {
			t.Errorf("GET %s: %d", p, code)
		}
	}
	if code, _ := e.do("GET", "/days/999", nil); code != 404 {
		t.Errorf("missing day: %d", code)
	}
}

func TestBasicAuth(t *testing.T) {
	e := newEnv(t, Options{AuthUser: "me", AuthPass: "secret"})
	if code, _ := e.do("GET", "/", nil); code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
	if code, _ := e.do("GET", "/healthz", nil); code != 200 {
		t.Fatalf("healthz must be open: %d", code)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/", nil)
	req.SetBasicAuth("me", "secret")
	resp, err := e.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("auth ok: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
}
