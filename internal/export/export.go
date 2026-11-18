// Package export — выгрузка дней в CSV/JSON/Markdown и разбор CSV для импорта.
package export

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"lifelog/internal/ru"
	"lifelog/internal/store"
)

// Columns — колонки CSV. Импорт принимает тот же формат (колонки по заголовку).
var Columns = []string{"date", "wake_at", "bed_at", "sleep_minutes", "train_start", "train_minutes", "train_type", "energy", "report"}

const sleepNote = "Сон = подъём этого дня − отбой предыдущего дня; пусто, если отбой не отмечен или пропущены дни."

// TZLabel — название пояса для людей и ИИ: «Europe/Berlin (UTC+01:00)».
func TZLabel(loc *time.Location) string {
	off := time.Now().In(loc).Format("-07:00")
	if loc.String() == "Local" {
		return "местное время сервера (UTC" + off + ")"
	}
	return loc.String() + " (UTC" + off + ")"
}

func tstr(t *time.Time, loc *time.Location) string {
	if t == nil {
		return ""
	}
	return t.In(loc).Format(time.RFC3339)
}

func istr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func sleepMinutes(d *time.Duration) *int {
	if d == nil {
		return nil
	}
	m := int(d.Round(time.Minute) / time.Minute)
	return &m
}

// WriteCSV пишет дни в CSV (времена — локальные, RFC 3339 со смещением).
func WriteCSV(w io.Writer, entries []store.Entry, loc *time.Location) error {
	cw := csv.NewWriter(w)
	cw.Write(Columns)
	for _, e := range entries {
		cw.Write([]string{
			e.LocalDate(loc),
			tstr(&e.WakeAt, loc),
			tstr(e.BedAt, loc),
			istr(sleepMinutes(e.Sleep)),
			tstr(e.TrainStart, loc),
			istr(e.TrainMinutes),
			e.TrainType,
			istr(e.Energy),
			e.Report,
		})
	}
	cw.Flush()
	return cw.Error()
}

type jsonDay struct {
	Date         string  `json:"date"`
	WakeAt       string  `json:"wake_at"`
	BedAt        *string `json:"bed_at"`
	SleepMinutes *int    `json:"sleep_minutes"`
	Sleep        *string `json:"sleep"`
	TrainStart   *string `json:"train_start"`
	TrainMinutes *int    `json:"train_minutes"`
	TrainType    *string `json:"train_type"`
	Energy       *int    `json:"energy"`
	Report       *string `json:"report"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// WriteJSON пишет дни в JSON.
func WriteJSON(w io.Writer, entries []store.Entry, loc *time.Location) error {
	days := make([]jsonDay, 0, len(entries))
	for _, e := range entries {
		jd := jsonDay{
			Date:         e.LocalDate(loc),
			WakeAt:       tstr(&e.WakeAt, loc),
			BedAt:        optStr(tstr(e.BedAt, loc)),
			SleepMinutes: sleepMinutes(e.Sleep),
			TrainStart:   optStr(tstr(e.TrainStart, loc)),
			TrainMinutes: e.TrainMinutes,
			TrainType:    optStr(e.TrainType),
			Energy:       e.Energy,
			Report:       optStr(e.Report),
		}
		if e.Sleep != nil {
			jd.Sleep = optStr(ru.Dur(e.Sleep))
		}
		days = append(days, jd)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(struct {
		Timezone string    `json:"timezone"`
		Note     string    `json:"note"`
		Days     []jsonDay `json:"days"`
	}{TZLabel(loc), sleepNote, days})
}

// WriteMarkdown пишет таблицу дней и отчёты — удобно вставить в чат с ИИ.
func WriteMarkdown(w io.Writer, entries []store.Entry, loc *time.Location) error {
	var b strings.Builder
	b.WriteString("# Журнал дня")
	if len(entries) > 0 {
		fmt.Fprintf(&b, ": %s — %s", entries[0].LocalDate(loc), entries[len(entries)-1].LocalDate(loc))
	}
	fmt.Fprintf(&b, "\n\nЧасовой пояс: %s. %s Энергия — по шкале 1–5.\n\n", TZLabel(loc), sleepNote)
	if len(entries) == 0 {
		b.WriteString("Записей нет.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	b.WriteString("| Дата | Подъём | Отбой | Сон | Тренировка | Энергия |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	hm := func(t *time.Time, base time.Time) string {
		if t == nil {
			return "—"
		}
		l := t.In(loc)
		s := l.Format("15:04")
		if l.Format("2006-01-02") != base.In(loc).Format("2006-01-02") {
			s += " (" + l.Format("02.01") + ")"
		}
		return s
	}
	for _, e := range entries {
		wake := e.WakeAt.In(loc)
		train := "—"
		if e.TrainType != "" || e.TrainMinutes != nil || e.TrainStart != nil {
			var parts []string
			if e.TrainType != "" {
				parts = append(parts, mdCell(e.TrainType))
			}
			if e.TrainStart != nil {
				parts = append(parts, "с "+hm(e.TrainStart, e.WakeAt))
			}
			if e.TrainMinutes != nil {
				parts = append(parts, fmt.Sprintf("%d мин", *e.TrainMinutes))
			}
			train = strings.Join(parts, ", ")
		}
		energy := "—"
		if e.Energy != nil {
			energy = strconv.Itoa(*e.Energy)
		}
		fmt.Fprintf(&b, "| %s %s | %s | %s | %s | %s | %s |\n",
			wake.Format("2006-01-02"), ru.Weekday(wake), wake.Format("15:04"), hm(e.BedAt, e.WakeAt), ru.Dur(e.Sleep), train, energy)
	}

	hasReports := false
	for _, e := range entries {
		if e.Report == "" {
			continue
		}
		if !hasReports {
			b.WriteString("\n## Отчёты\n")
			hasReports = true
		}
		wake := e.WakeAt.In(loc)
		fmt.Fprintf(&b, "\n### %s %s\n\n", wake.Format("2006-01-02"), ru.Weekday(wake))
		// Отступ сохраняет текст как кодовый блок, включая HTML и ссылки.
		report := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(e.Report)
		fmt.Fprintln(&b, "    "+strings.ReplaceAll(report, "\n", "\n    "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func mdCell(s string) string {
	var b strings.Builder
	for _, r := range strings.Join(strings.Fields(s), " ") {
		// Сущности отображаются как текст и не образуют синтаксис Markdown.
		switch {
		case '!' <= r && r <= '/', ':' <= r && r <= '@', '[' <= r && r <= '`', '{' <= r && r <= '~':
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
