package web

import (
	"io"
	"net/http"
	"strings"

	"lifelog/internal/export"
	"lifelog/internal/store"
)

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, "csv", "text/csv; charset=utf-8", func(w http.ResponseWriter, e []store.Entry) error {
		return export.WriteCSV(w, e, s.loc)
	})
}

func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, "json", "application/json; charset=utf-8", func(w http.ResponseWriter, e []store.Entry) error {
		return export.WriteJSON(w, e, s.loc)
	})
}

func (s *Server) exportMD(w http.ResponseWriter, r *http.Request) {
	s.export(w, r, "md", "text/markdown; charset=utf-8", func(w http.ResponseWriter, e []store.Entry) error {
		return export.WriteMarkdown(w, e, s.loc)
	})
}

func (s *Server) importCSV(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(20 << 20); err != nil && err != http.ErrNotMultipart {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var src io.Reader
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		src = f
	} else if text := r.FormValue("csv"); strings.TrimSpace(text) != "" {
		src = strings.NewReader(text)
	}

	n, _ := s.days(r).Count(r.Context())
	now := s.opt.Now().In(s.loc)
	data := dataPageData{
		From:  now.AddDate(0, 0, -29).Format("2006-01-02"),
		To:    now.Format("2006-01-02"),
		Count: n,
	}
	status := http.StatusOK
	switch rows, perr, err := parseOrNil(src, s); {
	case src == nil:
		data.Message = "Выбери файл или вставь CSV."
		status = http.StatusBadRequest
	case err != nil:
		data.Errors = []string{err.Error()}
		status = http.StatusBadRequest
	case len(perr) > 0:
		data.Errors = perr
		status = http.StatusBadRequest
	default:
		res, err := s.days(r).Import(r.Context(), rows)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(res.Errors) > 0 {
			data.Errors = res.Errors
			status = http.StatusBadRequest
		} else {
			data.Result = &res
			data.Count, _ = s.days(r).Count(r.Context())
		}
	}
	s.render(w, status, "data.html", page{Title: "Данные", Active: "data", Data: data})
}

func parseOrNil(src io.Reader, s *Server) ([]store.ImportRow, []string, error) {
	if src == nil {
		return nil, nil, nil
	}
	return export.ParseCSV(src, s.loc)
}
