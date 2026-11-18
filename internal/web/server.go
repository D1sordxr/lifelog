// Package web — HTTP-интерфейс: страницы, автосохранение, экспорт/импорт.
package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"lifelog/internal/store"
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
}

type Server struct {
	st      *store.Store
	loc     *time.Location
	opt     Options
	pages   map[string]*template.Template
	version string // хэш статики для сброса кэша
	mux     *http.ServeMux
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
	w.Header().Set("X-Frame-Options", "DENY")
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
			http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		}
	}()
	s.mux.ServeHTTP(w, r)
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
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	if err := t.ExecuteTemplate(&b, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "ошибка шаблона", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(b.String()))
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "ошибка: "+err.Error(), http.StatusInternalServerError)
}
