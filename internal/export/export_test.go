package export

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lifelog/internal/store"
)

var tz = time.FixedZone("UTC+4", 4*60*60)

func openTest(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "t.db"), tz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func importCSV(t *testing.T, s *store.Store, csv string) store.ImportResult {
	t.Helper()
	rows, perr, err := ParseCSV(strings.NewReader(csv), tz)
	if err != nil || len(perr) > 0 {
		t.Fatalf("parse: %v %v", err, perr)
	}
	res, err := s.For(1).Import(context.Background(), rows)
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("import: %v %v", err, res.Errors)
	}
	return res
}

const legacy = "\xEF\xBB\xBFdate,wake_at,bed_at,train_start,train_minutes,train_type,energy,report\n" +
	"2026-11-16,07:00,23:30,18:00,45,бег,4,\"Первый день, с запятой\"\n" +
	"17.11.2026,07:15,00:40,,,,3,\"Многострочный\nотчёт | с чертой\"\n" +
	"2026-11-18,08:00,,,,,,\n" +
	",,,,,,,\n"

func TestImportTimeOnlyAndSleep(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	res := importCSV(t, s, legacy)
	if res.Added != 3 || res.Updated != 0 {
		t.Fatalf("result %+v", res)
	}
	e, err := s.For(1).Range(ctx, time.Time{}, time.Time{})
	if err != nil || len(e) != 3 {
		t.Fatalf("range: %v %v", e, err)
	}
	// Отбой 00:40 после подъёма 07:15 → следующие сутки.
	if got := e[1].BedAt.In(tz).Format("2006-01-02 15:04"); got != "2026-11-18 00:40" {
		t.Fatalf("bed after midnight: %s", got)
	}
	if e[1].Sleep == nil || *e[1].Sleep != 7*time.Hour+45*time.Minute {
		t.Fatalf("sleep day 2: %v", e[1].Sleep)
	}
	if e[2].Sleep == nil || *e[2].Sleep != 7*time.Hour+20*time.Minute {
		t.Fatalf("sleep day 3: %v", e[2].Sleep)
	}
	if e[0].TrainType != "бег" || *e[0].TrainMinutes != 45 || e[0].TrainStart.In(tz).Hour() != 18 {
		t.Fatalf("train: %+v", e[0].Day)
	}

	// Повторный импорт той же даты обновляет, а не дублирует; пустые ячейки не стирают.
	res = importCSV(t, s, "date,wake_at,energy\n2026-11-16,06:50,5\n")
	if res.Added != 0 || res.Updated != 1 {
		t.Fatalf("reimport %+v", res)
	}
	d, _ := s.For(1).Range(ctx, time.Time{}, time.Time{})
	if *d[0].Energy != 5 || d[0].TrainType != "бег" || d[0].WakeAt.In(tz).Format("15:04") != "06:50" {
		t.Fatalf("after update: %+v", d[0].Day)
	}
}

func TestCSVRoundTrip(t *testing.T) {
	a := openTest(t)
	ctx := context.Background()
	importCSV(t, a, legacy)
	src, _ := a.For(1).Range(ctx, time.Time{}, time.Time{})

	var buf bytes.Buffer
	if err := WriteCSV(&buf, src, tz); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "sleep_minutes") || !strings.Contains(buf.String(), ",465,") {
		t.Fatalf("csv must contain sleep minutes (465):\n%s", buf.String())
	}

	b := openTest(t)
	importCSV(t, b, buf.String())
	dst, _ := b.For(1).Range(ctx, time.Time{}, time.Time{})
	if len(dst) != len(src) {
		t.Fatalf("len %d != %d", len(dst), len(src))
	}
	for i := range src {
		x, y := src[i], dst[i]
		same := x.WakeAt.Equal(y.WakeAt) && eqT(x.BedAt, y.BedAt) && eqT(x.TrainStart, y.TrainStart) &&
			eqI(x.TrainMinutes, y.TrainMinutes) && eqI(x.Energy, y.Energy) &&
			x.TrainType == y.TrainType && x.Report == y.Report && eqD(x.Sleep, y.Sleep)
		if !same {
			t.Errorf("day %d differs:\n%+v\n%+v", i, x, y)
		}
	}
}

func TestMarkdownAndJSON(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	importCSV(t, s, legacy)
	e, _ := s.For(1).Range(ctx, time.Time{}, time.Time{})

	var md bytes.Buffer
	WriteMarkdown(&md, e, tz)
	out := md.String()
	for _, want := range []string{
		"# Журнал дня: 2026-11-16 — 2026-11-18",
		"| 2026-11-17 Вт | 07:15 | 00:40 (18.11) | 7:45 | — | 3 |",
		"| 2026-11-18 Ср | 08:00 | — | 7:20 |",
		"бег, с 18:00, 45 мин",
		"### 2026-11-17 Вт\n\nМногострочный\nотчёт | с чертой",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown lacks %q:\n%s", want, out)
		}
	}

	var js bytes.Buffer
	WriteJSON(&js, e, tz)
	var parsed struct {
		Days []struct {
			Date         string  `json:"date"`
			SleepMinutes *int    `json:"sleep_minutes"`
			Sleep        *string `json:"sleep"`
			BedAt        *string `json:"bed_at"`
		} `json:"days"`
	}
	if err := json.Unmarshal(js.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Days) != 3 || parsed.Days[0].SleepMinutes != nil || *parsed.Days[1].SleepMinutes != 465 ||
		*parsed.Days[1].Sleep != "7:45" || parsed.Days[2].BedAt != nil {
		t.Fatalf("json: %s", js.String())
	}
}

func TestParseErrors(t *testing.T) {
	if _, _, err := ParseCSV(strings.NewReader("date,wake,bed\n"), tz); err == nil {
		t.Error("unknown column must fail")
	}
	if _, _, err := ParseCSV(strings.NewReader("date,energy\n"), tz); err == nil {
		t.Error("missing wake_at must fail")
	}
	_, perr, _ := ParseCSV(strings.NewReader("wake_at,energy\n07:00,3\n2026-11-18 07:00,3\n"), tz)
	if len(perr) != 1 || !strings.Contains(perr[0], "строка 2") {
		t.Errorf("time without date: %v", perr)
	}
	rows, perr, _ := ParseCSV(strings.NewReader("date;wake_at;energy\n18.11.2026;07:00;3\n"), tz)
	if len(perr) != 0 || len(rows) != 1 {
		t.Errorf("semicolon csv: %v %v", rows, perr)
	}

	// Ошибка валидации в одной строке → не импортируется ничего.
	s := openTest(t)
	rows, _, _ = ParseCSV(strings.NewReader("date,wake_at,energy\n2026-11-17,07:00,3\n2026-11-18,07:00,9\n"), tz)
	res, err := s.For(1).Import(context.Background(), rows)
	if err != nil || len(res.Errors) != 1 {
		t.Fatalf("want 1 error, got %v %v", res, err)
	}
	if n, _ := s.For(1).Count(context.Background()); n != 0 {
		t.Fatalf("partial import: %d days", n)
	}
}

func eqT(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
func eqI(a, b *int) bool       { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
func eqD(a, b *time.Duration) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}
