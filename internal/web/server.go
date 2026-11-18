// Package web — HTTP-интерфейс: страницы, автосохранение, экспорт/импорт.
package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"lifelog/internal/store"
	"lifelog/internal/tg"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Options — настройки сервера.
type Options struct {
	// Если оба заданы — включается HTTP basic auth.
	AuthUser, AuthPass string
	// Now — часы (для тестов). По умолчанию time.Now.
	Now func() time.Time
	// BotToken включает вход через Telegram Mini App: без него — один
	// пользователь без входа. Allow — кому разрешён вход.
	BotToken string
	Allow    tg.Allowlist
}

type Server struct {
	st      *store.Store
	loc     *time.Location
	opt     Options
	pages   map[string]*template.Template
	version string // хэш статики для сброса кэша
	mux     *http.ServeMux
	handler http.Handler
	cop     *http.CrossOriginProtection
	sessKey []byte
}

// New собирает обработчик.
func New(st *store.Store, opt Options) *Server {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	s := &Server{st: st, loc: st.Loc(), opt: opt, mux: http.NewServeMux()}
	s.version = staticVersion()
	s.parseTemplates()
	s.routes()
	s.sessKey = sessionKey(opt.BotToken)
	// POST только со своего origin (Sec-Fetch-Site/Origin) — защита от CSRF.
	s.cop = http.NewCrossOriginProtection()
	s.handler = s.mux
	return s
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.today)
	m.HandleFunc("POST /wake", s.wake)
	m.HandleFunc("GET /days/{id}", s.dayPage)
	m.HandleFunc("POST /days/{id}", s.saveForm)
	m.HandleFunc("POST /days/{id}/field", s.saveField)
	m.HandleFunc("POST /days/{id}/bed", s.bed)
	m.HandleFunc("POST /days/{id}/delete", s.deleteDay)
	m.HandleFunc("GET /history", s.history)
	m.HandleFunc("GET /chart", s.chart)
	m.HandleFunc("GET /data", s.dataPage)
	m.HandleFunc("POST /import", s.importCSV)
	m.HandleFunc("GET /export.csv", s.exportCSV)
	m.HandleFunc("GET /export.json", s.exportJSON)
	m.HandleFunc("GET /export.md", s.exportMD)
	m.HandleFunc("POST /auth/telegram", s.authTelegram)
	m.HandleFunc("POST /auth/download", s.downloadToken)
	m.HandleFunc("GET /pin", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", http.StatusSeeOther) })
	m.HandleFunc("POST /pin", s.pinSubmit)
	m.HandleFunc("POST /pin/lock", s.pinLock)
	m.HandleFunc("GET /pin/change", s.pinChangePage)
	m.HandleFunc("POST /pin/change", s.pinChange)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(files)))
	m.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, static, "manifest.json")
	})
	for _, p := range []string{"/apple-touch-icon.png", "/apple-touch-icon-precomposed.png", "/favicon.ico"} {
		m.HandleFunc("GET "+p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			http.ServeFileFS(w, r, static, "icon-180.png")
		})
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Telegram Web открывает мини-приложение во фрейме: разрешаем только его
	// (X-Frame-Options: DENY давал пустое окно).
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'self' https://web.telegram.org https://*.telegram.org")
	if s.opt.AuthUser != "" && s.opt.AuthPass != "" && r.URL.Path != "/healthz" {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(s.opt.AuthUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(s.opt.AuthPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="lifelog", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	defer func() {
		if v := recover(); v != nil {
			log.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
			http.Error(w, internalError, http.StatusInternalServerError)
		}
	}()
	if err := s.cop.Check(r); err != nil {
		http.Error(w, "запрос с чужого сайта", http.StatusForbidden)
		return
	}
	r, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	s.handler.ServeHTTP(w, r)
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}

func staticVersion() string {
	h := sha256.New()
	fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := staticFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}

func (s *Server) parseTemplates() {
	funcs := template.FuncMap{
		"static": func(name string) string { return "/static/" + name + "?v=" + s.version },
		"lines":  func(v string) []string { return strings.Split(v, "\n") },
	}
	s.pages = map[string]*template.Template{}
	names, _ := fs.Glob(templatesFS, "templates/*.html")
	for _, n := range names {
		base := strings.TrimPrefix(n, "templates/")
		if base == "layout.html" || strings.HasPrefix(base, "_") {
			continue
		}
		s.pages[base] = template.Must(template.New("").Funcs(funcs).ParseFS(templatesFS,
			"templates/layout.html", "templates/_*.html", n))
	}
}

// page — общие данные для layout.html.
type page struct {
	Title  string
	Active string
	Data   any
	Bare   bool   // без навигации (вход, PIN)
	Login  bool   // экран входа без сессии
	TG     bool   // подключить telegram-web-app.js
	Script string // скрипт страницы, по умолчанию app.js
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		s.fail(w, errors.New("нет шаблона "+name))
		return
	}
	p.TG = s.tgMode()
	if p.Script == "" && !p.Login {
		p.Script = "app.js"
	}
	var b strings.Builder
	if err := t.ExecuteTemplate(&b, "layout", p); err != nil {
		s.fail(w, fmt.Errorf("render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(b.String()))
}

const internalError = "внутренняя ошибка"

func (s *Server) logError(err error) { log.Printf("error: %v", err) }

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logError(err)
	http.Error(w, internalError, http.StatusInternalServerError)
}

// readSmallForm ограничивает тело до разбора и не принимает multipart.
func readSmallForm(w http.ResponseWriter, r *http.Request, limit int64) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "ожидается форма"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "форма слишком большая"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "не удалось прочитать форму"})
		}
		return false
	}
	return true
}
