package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lifelog/internal/ru"
	"lifelog/internal/store"
)

// dayView — день, подготовленный для шаблона формы.
type dayView struct {
	UserID     int64
	ID         int64
	DateLong   string
	Values     map[string]string
	Energy     int
	Sleep      string
	ShowBed    bool
	Stale      bool
	TrainTypes []string
	PrevID     int64
	NextID     int64
}

var energyOpts = []int{1, 2, 3, 4, 5}

func (s *Server) view(r *http.Request, d *store.Day) (*dayView, error) {
	v := &dayView{
		ID:       d.ID,
		UserID:   s.days(r).UserID(),
		DateLong: ru.DateLong(d.WakeAt.In(s.loc)),
		Values:   map[string]string{},
	}
	for _, f := range store.Fields {
		v.Values[f.Name] = f.Get(d, s.loc)
	}
	if d.Energy != nil {
		v.Energy = *d.Energy
	}
	sl, err := s.days(r).SleepBefore(r.Context(), d)
	if err != nil {
		return nil, err
	}
	v.Sleep = ru.Dur(sl)
	v.Stale = d.BedAt == nil && s.opt.Now().Sub(d.WakeAt) > store.StaleAfter
	if v.TrainTypes, err = s.days(r).TrainTypes(r.Context()); err != nil {
		return nil, err
	}
	return v, nil
}

func (s *Server) dayID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *Server) hm(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.In(s.loc).Format("15:04")
}

// --- Сегодня ---

type todayData struct {
	Day      *dayView
	Energies []int
	LastID   int64
	LastDate string
	LastBed  string
}

func (s *Server) today(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	latest, err := s.days(r).Latest(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := todayData{Energies: energyOpts}
	if latest != nil && latest.BedAt == nil {
		if data.Day, err = s.view(r, latest); err != nil {
			s.fail(w, err)
			return
		}
	} else if latest != nil {
		data.LastID = latest.ID
		data.LastDate = ru.DateLong(latest.WakeAt.In(s.loc))
		data.LastBed = s.hm(latest.BedAt)
	}
	s.render(w, http.StatusOK, "today.html", page{Title: "Сегодня", Active: "today", Data: data})
}

func (s *Server) wake(w http.ResponseWriter, r *http.Request) {
	if _, err := s.days(r).Wake(r.Context(), s.opt.Now()); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) bed(w http.ResponseWriter, r *http.Request) {
	id, ok := s.dayID(w, r)
	if !ok {
		return
	}
	if err := s.days(r).Bed(r.Context(), id, s.opt.Now()); err != nil {
		s.inputOrFail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- Страница дня ---

type dayPageData struct {
	Day      *dayView
	Energies []int
}

func (s *Server) dayPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.dayID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	d, err := s.days(r).Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	v, err := s.view(r, d)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.ShowBed = true
	v.Stale = false
	if p, err := s.days(r).Prev(ctx, d); err == nil && p != nil {
		v.PrevID = p.ID
	}
	if n, err := s.days(r).Next(ctx, d); err == nil && n != nil {
		v.NextID = n.ID
	}
	s.render(w, http.StatusOK, "day.html", page{Title: v.DateLong, Active: "history", Data: dayPageData{v, energyOpts}})
}

// saveForm — сохранение формы целиком (без JS).
func (s *Server) saveForm(w http.ResponseWriter, r *http.Request) {
	id, ok := s.dayID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	values := map[string]string{}
	for _, f := range store.Fields {
		if _, ok := r.PostForm[f.Name]; ok {
			values[f.Name] = r.PostForm.Get(f.Name)
		}
	}
	if _, err := s.days(r).SetFields(r.Context(), id, values); err != nil {
		s.inputOrFail(w, err)
		return
	}
	back := r.PostForm.Get("back")
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
		back = "/days/" + strconv.FormatInt(id, 10)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// saveField — автосохранение одного поля. Ответ JSON.
func (s *Server) saveField(w http.ResponseWriter, r *http.Request) {
	id, ok := s.dayID(w, r)
	if !ok {
		return
	}
	reply := func(status int, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	if err := r.ParseForm(); err != nil {
		reply(http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	name := r.PostForm.Get("name")
	d, err := s.days(r).SetField(r.Context(), id, name, r.PostForm.Get("value"))
	var ie *store.InputError
	switch {
	case errors.Is(err, store.ErrNotFound):
		reply(http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	case errors.As(err, &ie):
		reply(http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	case err != nil:
		s.logError(err)
		reply(http.StatusInternalServerError, map[string]any{"error": internalError})
		return
	}
	f, _ := store.FieldByName(name)
	sl, _ := s.days(r).SleepBefore(r.Context(), d)
	reply(http.StatusOK, map[string]any{"ok": true, "value": f.Get(d, s.loc), "sleep": ru.Dur(sl)})
}

func (s *Server) deleteDay(w http.ResponseWriter, r *http.Request) {
	id, ok := s.dayID(w, r)
	if !ok {
		return
	}
	if err := s.days(r).Delete(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/history", http.StatusSeeOther)
}

func (s *Server) inputOrFail(w http.ResponseWriter, err error) {
	var ie *store.InputError
	if errors.As(err, &ie) {
		http.Error(w, "Ошибка: "+err.Error()+"\n\nВернись назад и исправь.", http.StatusBadRequest)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.fail(w, err)
}

// --- История ---

const historyPageSize = 50

type historyRow struct {
	ID                                    int64
	Date, Wake, Bed, Sleep, Train, Report string
	Energy                                int
	Open                                  bool
}

type historyData struct {
	Rows     []historyRow
	Page     int
	PrevPage int
	NextPage int
	HasPrev  bool
	HasNext  bool
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pg, _ := strconv.Atoi(r.URL.Query().Get("p"))
	if pg < 0 {
		pg = 0
	}
	entries, err := s.days(r).Page(ctx, historyPageSize, pg*historyPageSize)
	if err != nil {
		s.fail(w, err)
		return
	}
	total, err := s.days(r).Count(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.opt.Now().In(s.loc)
	data := historyData{Page: pg, PrevPage: pg - 1, NextPage: pg + 1, HasPrev: pg > 0, HasNext: (pg+1)*historyPageSize < total}
	for _, e := range entries {
		row := historyRow{
			ID:     e.ID,
			Date:   ru.DateShort(e.WakeAt.In(s.loc), now),
			Wake:   s.hm(&e.WakeAt),
			Bed:    s.hm(e.BedAt),
			Sleep:  ru.Dur(e.Sleep),
			Train:  trainSummary(&e.Day, s.loc),
			Report: snippet(e.Report, 120),
			Open:   e.BedAt == nil,
		}
		if e.Energy != nil {
			row.Energy = *e.Energy
		}
		data.Rows = append(data.Rows, row)
	}
	s.render(w, http.StatusOK, "history.html", page{Title: "История", Active: "history", Data: data})
}

func trainSummary(d *store.Day, loc *time.Location) string {
	var parts []string
	if d.TrainType != "" {
		parts = append(parts, d.TrainType)
	} else if d.TrainMinutes != nil || d.TrainStart != nil {
		parts = append(parts, "тренировка")
	}
	if d.TrainStart != nil {
		parts = append(parts, d.TrainStart.In(loc).Format("15:04"))
	}
	if d.TrainMinutes != nil {
		parts = append(parts, fmt.Sprintf("%d мин", *d.TrainMinutes))
	}
	return strings.Join(parts, " · ")
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

// --- Данные: экспорт/импорт ---

type dataPageData struct {
	PIN      bool // показать «Сменить PIN»
	From, To string
	Count    int
	Result   *store.ImportResult
	Errors   []string
	Message  string
}

func (s *Server) dataPage(w http.ResponseWriter, r *http.Request) {
	n, err := s.days(r).Count(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.opt.Now().In(s.loc)
	d := dataPageData{
		PIN:   s.tgMode(),
		From:  now.AddDate(0, 0, -29).Format("2006-01-02"),
		To:    now.Format("2006-01-02"),
		Count: n,
	}
	s.render(w, http.StatusOK, "data.html", page{Title: "Данные", Active: "data", Data: d})
}

// rangeParams разбирает ?from=&to= (локальные даты, включительно).
func (s *Server) rangeParams(q url.Values) (from, to time.Time, label string, err error) {
	label = "all"
	if v := q.Get("from"); v != "" {
		if from, _, err = store.DayBounds(v, s.loc); err != nil {
			return from, to, "", fmt.Errorf("from: нужна дата ГГГГ-ММ-ДД")
		}
		label = v
	}
	if v := q.Get("to"); v != "" {
		if _, to, err = store.DayBounds(v, s.loc); err != nil {
			return from, to, "", fmt.Errorf("to: нужна дата ГГГГ-ММ-ДД")
		}
		if label == "all" {
			label = "start"
		}
		label += "_" + v
	}
	return from, to, label, nil
}

func (s *Server) export(w http.ResponseWriter, r *http.Request, ext, ctype string, write func(http.ResponseWriter, []store.Entry) error) {
	from, to, label, err := s.rangeParams(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries, err := s.days(r).Range(r.Context(), from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("inline") != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lifelog-%s.%s"`, url.PathEscape(label), ext))
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := write(w, entries); err != nil {
		s.fail(w, err)
	}
}
