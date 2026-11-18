package export

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"lifelog/internal/store"
)

// ParseCSV разбирает CSV в формате экспорта. Колонки определяются по заголовку,
// порядок и набор произвольные; обязательна wake_at. Пустые ячейки игнорируются.
//
// Времена: полные (2026-11-18T07:30:00+03:00, 2026-11-18 07:30) или только
// «ЧЧ:ММ» — тогда нужна колонка date (2026-11-18 или 18.11.2026). Отбой и начало
// тренировки «ЧЧ:ММ», которые раньше подъёма, относятся к следующим суткам.
func ParseCSV(r io.Reader, loc *time.Location) ([]store.ImportRow, []string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 20<<20))
	if err != nil {
		return nil, nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	cr := csv.NewReader(bytes.NewReader(data))
	firstLine, _, _ := bytes.Cut(data, []byte("\n"))
	if bytes.Contains(firstLine, []byte(";")) && !bytes.Contains(firstLine, []byte(",")) {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true

	header, err := cr.Read()
	if err != nil {
		return nil, nil, errors.New("пустой файл или нет заголовка")
	}
	known := map[string]bool{}
	for _, c := range Columns {
		known[c] = true
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		if !known[h] {
			return nil, nil, fmt.Errorf("неизвестная колонка %q (допустимы: %s)", h, strings.Join(Columns, ", "))
		}
		col[h] = i
	}
	if _, ok := col["wake_at"]; !ok {
		return nil, nil, errors.New("нет колонки wake_at")
	}

	var rows []store.ImportRow
	var errs []string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line, _ := cr.FieldPos(0)
		if err != nil {
			errs = append(errs, fmt.Sprintf("строка %d: %v", line, err))
			continue
		}
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if strings.TrimSpace(strings.Join(rec, "")) == "" {
			continue
		}
		row, err := parseRow(get, loc)
		if err != nil {
			errs = append(errs, fmt.Sprintf("строка %d: %v", line, err))
			continue
		}
		row.Line = line
		rows = append(rows, row)
	}
	return rows, errs, nil
}

func parseRow(get func(string) string, loc *time.Location) (store.ImportRow, error) {
	row := store.ImportRow{Values: map[string]string{}}
	var base *time.Time
	if ds := get("date"); ds != "" {
		d, err := parseDate(ds, loc)
		if err != nil {
			return row, err
		}
		base = &d
	}
	wake, err := parseFlexTime(get("wake_at"), base, nil, loc)
	if err != nil {
		return row, fmt.Errorf("wake_at: %w", err)
	}
	if wake == nil {
		return row, errors.New("wake_at: пусто")
	}
	row.WakeAt = *wake
	if base == nil {
		b := dateOf(*wake, loc)
		base = &b
	}
	for _, name := range []string{"bed_at", "train_start"} {
		t, err := parseFlexTime(get(name), base, wake, loc)
		if err != nil {
			return row, fmt.Errorf("%s: %w", name, err)
		}
		if t != nil {
			row.Values[name] = t.Format(time.RFC3339)
		}
	}
	for _, name := range []string{"train_minutes", "train_type", "energy", "report"} {
		if v := get(name); v != "" {
			row.Values[name] = v
		}
	}
	return row, nil
}

func dateOf(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

func parseDate(s string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", "02.01.2006", "2.1.2006"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("не понимаю дату %q", s)
}

// parseFlexTime: полная дата-время или «ЧЧ:ММ» относительно base.
// Если after задан и время «ЧЧ:ММ» получилось не позже него — сдвиг на сутки.
func parseFlexTime(s string, base, after *time.Time, loc *time.Location) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05", "02.01.2006 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return &t, nil
		}
	}
	for _, layout := range []string{"15:04", "15.04"} {
		hm, err := time.Parse(layout, s)
		if err != nil {
			continue
		}
		if base == nil {
			return nil, fmt.Errorf("время %q без даты: добавь колонку date", s)
		}
		t := time.Date(base.Year(), base.Month(), base.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
		if after != nil && !t.After(*after) {
			t = t.AddDate(0, 0, 1)
		}
		return &t, nil
	}
	return nil, fmt.Errorf("не понимаю время %q", s)
}
