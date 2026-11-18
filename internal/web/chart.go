package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lifelog/internal/ru"
	"lifelog/internal/store"
)

// Ось Y графика — сутки от 18:00 предыдущего дня до 18:00 дня подъёма,
// чтобы ночь не разрывалась на полуночи.
const anchorHour = 18

type chartData struct {
	Days       int
	Options    []int
	SVG        template.HTML
	Nights     int
	AvgSleep   string
	AvgBed     string
	AvgWake    string
	SpreadBed  string
	SpreadWake string
}

func (s *Server) chart(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("d"))
	if n != 60 && n != 90 {
		n = 30
	}
	today := s.opt.Now().In(s.loc)
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, s.loc).AddDate(0, 0, -(n - 1))
	end := start.AddDate(0, 0, n)
	entries, err := s.days(r).Range(r.Context(), start, end)
	if err != nil {
		s.fail(w, err)
		return
	}
	data := buildChart(entries, start, n, s.loc)
	data.Options = []int{30, 60, 90}
	s.render(w, http.StatusOK, "chart.html", page{Title: "График сна", Active: "chart", Data: data})
}

func buildChart(entries []store.Entry, start time.Time, n int, loc *time.Location) chartData {
	const (
		W, H           = 640.0, 620.0
		ml, mr, mt, mb = 60.0, 28.0, 12.0, 38.0
	)
	pw, ph := W-ml-mr, H-mt-mb
	colW := pw / float64(n)
	y := func(min float64) float64 {
		return mt + math.Max(0, math.Min(1440, min))/1440*ph
	}
	// offset — минуты от 18:00 накануне даты day. На переходе DST
	// шкала прошедшего времени может расходиться с местными часами.
	offset := func(t time.Time, day time.Time) float64 {
		a := time.Date(day.Year(), day.Month(), day.Day()-1, anchorHour, 0, 0, 0, loc)
		return t.Sub(a).Minutes()
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %.0f %.0f" role="img" aria-label="Сон по дням">`, W, H)
	for h := 0; h <= 24; h += 3 {
		yy := y(float64(h * 60))
		cls := "grid"
		if (anchorHour+h)%24 == 0 {
			cls = "grid mid"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, cls, ml, W-mr, yy, yy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" dominant-baseline="middle">%02d:00</text>`, ml-8, yy, (anchorHour+h)%24)
	}
	step := n / 6
	for i := 0; i < n; i++ {
		if (n-1-i)%step != 0 {
			continue
		}
		d := start.AddDate(0, 0, i)
		x := ml + (float64(i)+0.5)*colW
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, H-10, d.Format("02.01"))
	}

	var (
		cd                = chartData{Days: n}
		sleepSum          time.Duration
		wakeOffs, bedOffs []float64
	)
	for _, e := range entries {
		wake := e.WakeAt.In(loc)
		day := time.Date(wake.Year(), wake.Month(), wake.Day(), 0, 0, 0, 0, loc)
		i := int(math.Round(day.Sub(start).Hours() / 24))
		if i < 0 || i >= n {
			continue
		}
		x := ml + float64(i)*colW
		wo := offset(e.WakeAt, day)
		wakeOffs = append(wakeOffs, wo)
		if e.BedAt != nil {
			bedOffs = append(bedOffs, offset(*e.BedAt, day.AddDate(0, 0, 1)))
		}
		if e.Sleep != nil {
			cd.Nights++
			sleepSum += *e.Sleep
			bo := offset(e.WakeAt.Add(-*e.Sleep), day)
			bw := math.Max(colW*0.7, 1.5)
			y1, y2 := y(bo), y(wo)
			fmt.Fprintf(&b, `<rect class="bar" x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.1f"><title>%s: %s–%s, %s</title></rect>`,
				x+(colW-bw)/2, y1, bw, math.Max(y2-y1, 1), math.Min(bw/2, 3),
				wake.Format("02.01"), e.WakeAt.Add(-*e.Sleep).In(loc).Format("15:04"), wake.Format("15:04"), ru.Dur(e.Sleep))
		} else {
			// Сон неизвестен — отмечаем хотя бы подъём.
			fmt.Fprintf(&b, `<circle class="dot" cx="%.1f" cy="%.1f" r="%.1f"><title>%s: подъём %s</title></circle>`,
				x+colW/2, y(wo), math.Max(math.Min(colW*0.35, 4), 2), wake.Format("02.01"), wake.Format("15:04"))
		}
	}
	b.WriteString(`</svg>`)
	cd.SVG = template.HTML(b.String())

	if cd.Nights > 0 {
		avg := sleepSum / time.Duration(cd.Nights)
		cd.AvgSleep = ru.Dur(&avg)
	}
	cd.AvgWake, cd.SpreadWake = clockStats(wakeOffs)
	cd.AvgBed, cd.SpreadBed = clockStats(bedOffs)
	return cd
}

// clockStats — среднее время суток и стандартное отклонение (в минутах)
// для смещений от 18:00.
func clockStats(offs []float64) (string, string) {
	if len(offs) == 0 {
		return "", ""
	}
	var sum float64
	for _, o := range offs {
		sum += o
	}
	mean := sum / float64(len(offs))
	var sq float64
	for _, o := range offs {
		sq += (o - mean) * (o - mean)
	}
	sd := math.Sqrt(sq / float64(len(offs)))
	m := (int(math.Round(mean)) + anchorHour*60) % 1440
	if m < 0 {
		m += 1440
	}
	return fmt.Sprintf("%02d:%02d", m/60, m%60), fmt.Sprintf("±%d мин", int(math.Round(sd)))
}
