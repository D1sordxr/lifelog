package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var tz = time.FixedZone("UTC+4", 4*60*60)

var zeroT time.Time

// lt — локальное время в tz.
func lt(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, tz)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func ptr[T any](v T) *T { return &v }

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), tz)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSleepBetween(t *testing.T) {
	cases := []struct {
		name string
		prev *Day
		wake string
		want time.Duration // 0 = nil
	}{
		{"нет предыдущего дня", nil, "2026-11-18 08:00", 0},
		{"обычная ночь", &Day{BedAt: ptr(lt("2026-11-17 23:30"))}, "2026-11-18 07:15", 7*time.Hour + 45*time.Minute},
		{"отбой после полуночи", &Day{BedAt: ptr(lt("2026-11-18 01:30"))}, "2026-11-18 09:00", 7*time.Hour + 30*time.Minute},
		{"у предыдущего нет отбоя", &Day{}, "2026-11-18 08:00", 0},
		{"пропущены дни", &Day{BedAt: ptr(lt("2026-11-15 23:00"))}, "2026-11-18 08:00", 0},
		{"отбой позже подъёма (ошибка ввода)", &Day{BedAt: ptr(lt("2026-11-18 09:00"))}, "2026-11-18 08:00", 0},
		{"длинный сон в пределах лимита", &Day{BedAt: ptr(lt("2026-11-17 20:00"))}, "2026-11-18 12:00", 16 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SleepBetween(c.prev, &Day{WakeAt: lt(c.wake)})
			switch {
			case c.want == 0 && got != nil:
				t.Fatalf("want nil, got %v", *got)
			case c.want != 0 && (got == nil || *got != c.want):
				t.Fatalf("want %v, got %v", c.want, got)
			}
		})
	}
}

func TestWithSleepChain(t *testing.T) {
	days := []Day{
		{WakeAt: lt("2026-11-10 07:00"), BedAt: ptr(lt("2026-11-10 23:00"))},
		{WakeAt: lt("2026-11-11 07:00"), BedAt: ptr(lt("2026-11-12 00:30"))}, // лёг после полуночи
		{WakeAt: lt("2026-11-12 08:00")},                                     // забыл «Лёг»
		{WakeAt: lt("2026-11-13 07:30"), BedAt: ptr(lt("2026-11-13 23:30"))},
		// 14-е и 15-е пропущены
		{WakeAt: lt("2026-11-16 07:00")},
	}
	e := WithSleep(nil, days)
	want := []time.Duration{0, 8 * time.Hour, 7*time.Hour + 30*time.Minute, 0, 0}
	for i, w := range want {
		got := e[i].Sleep
		if (w == 0) != (got == nil) || (got != nil && *got != w) {
			t.Errorf("day %d: want %v, got %v", i, w, got)
		}
	}
}

func TestCurrentDay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if d, err := s.For(1).Current(ctx); err != nil || d != nil {
		t.Fatalf("пустая база: want nil, got %v %v", d, err)
	}

	d1, err := s.For(1).Wake(ctx, lt("2026-11-17 07:00"))
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.For(1).Current(ctx); cur == nil || cur.ID != d1.ID {
		t.Fatalf("после подъёма текущий должен быть d1, got %v", cur)
	}

	s.SetClock(func() time.Time { return lt("2026-11-17 23:40") })
	if _, err := s.For(1).SetField(ctx, d1.ID, "bed_at", "now"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.For(1).Current(ctx); cur != nil {
		t.Fatalf("после отбоя текущего дня нет, got %v", cur)
	}

	// Старый день без отбоя + новый закрытый: текущего нет.
	old, _ := s.For(1).Wake(ctx, lt("2026-11-10 07:00"))
	_ = old
	if cur, _ := s.For(1).Current(ctx); cur != nil {
		t.Fatalf("старый открытый день не должен становиться текущим, got %v", cur)
	}

	d2, _ := s.For(1).Wake(ctx, lt("2026-11-18 07:30"))
	if cur, _ := s.For(1).Current(ctx); cur == nil || cur.ID != d2.ID {
		t.Fatalf("want d2, got %v", cur)
	}
	sl, _ := s.For(1).SleepBefore(ctx, d2)
	if sl == nil || *sl != 7*time.Hour+50*time.Minute {
		t.Fatalf("сон перед d2: want 7h50m, got %v", sl)
	}
}

func TestWakeDoubleTap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.For(1).Wake(ctx, lt("2026-11-18 07:00"))
	b, _ := s.For(1).Wake(ctx, lt("2026-11-18 07:00").Add(30*time.Second))
	if a.ID != b.ID {
		t.Fatal("двойное нажатие создало второй день")
	}
	if n, _ := s.For(1).Count(ctx); n != 1 {
		t.Fatalf("want 1 day, got %d", n)
	}
}

func TestSetFieldValidation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _ := s.For(1).Wake(ctx, lt("2026-11-18 07:00"))
	for _, c := range []struct{ name, val string }{
		{"energy", "6"},
		{"energy", "abc"},
		{"train_minutes", "-5"},
		{"bed_at", "2026-11-18T06:00"},
		{"wake_at", ""},
		{"nope", "1"},
	} {
		if _, err := s.For(1).SetField(ctx, d.ID, c.name, c.val); err == nil {
			t.Errorf("%s=%q: want error", c.name, c.val)
		}
	}
	got, err := s.For(1).SetField(ctx, d.ID, "energy", "4")
	if err != nil || got.Energy == nil || *got.Energy != 4 {
		t.Fatalf("energy=4: %v %v", got, err)
	}
	got, _ = s.For(1).SetField(ctx, d.ID, "energy", "")
	if got.Energy != nil {
		t.Fatal("пустое значение должно очищать поле")
	}
	got, _ = s.For(1).SetField(ctx, d.ID, "bed_at", "2026-11-19T00:45") // после полуночи
	if got.BedAt == nil || !got.BedAt.Equal(lt("2026-11-19 00:45")) {
		t.Fatalf("bed_at: %v", got.BedAt)
	}
	reread, _ := s.For(1).Get(ctx, d.ID)
	if !reread.BedAt.Equal(*got.BedAt) || !reread.WakeAt.Equal(d.WakeAt) {
		t.Fatal("после перечитывания из базы времена не совпали")
	}
}

func TestRangeUsesPrevDay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d1, _ := s.For(1).Wake(ctx, lt("2026-11-17 07:00"))
	s.For(1).SetField(ctx, d1.ID, "bed_at", "2026-11-17T23:00")
	s.For(1).Wake(ctx, lt("2026-11-18 07:00"))

	from, to, _ := DayBounds("2026-11-18", tz)
	e, err := s.For(1).Range(ctx, from, to)
	if err != nil || len(e) != 1 {
		t.Fatalf("range: %v %v", e, err)
	}
	if e[0].Sleep == nil || *e[0].Sleep != 8*time.Hour {
		t.Fatalf("сон должен считаться по дню вне диапазона, got %v", e[0].Sleep)
	}

	page, _ := s.For(1).Page(ctx, 1, 0)
	if len(page) != 1 || page[0].Sleep == nil || *page[0].Sleep != 8*time.Hour {
		t.Fatalf("page: %+v", page)
	}
}

func TestDailyBackup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.For(1).Wake(ctx, lt("2026-11-18 07:00"))
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		day := lt("2026-11-01 12:00").AddDate(0, 0, i)
		s.SetClock(func() time.Time { return day })
		if _, err := s.DailyBackup(ctx, dir, 3); err != nil {
			t.Fatal(err)
		}
	}
	// повторный вызов в тот же день ничего не создаёт
	if p, _ := s.DailyBackup(ctx, dir, 3); p != "" {
		t.Fatalf("повторная копия: %s", p)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	if len(files) != 3 || filepath.Base(files[0]) != "lifelog-2026-11-03.db" {
		t.Fatalf("files: %v", files)
	}
	b, err := Open(files[2], tz)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if n, _ := b.For(1).Count(ctx); n != 1 {
		t.Fatalf("в копии %d дней", n)
	}
}

// Первый запрос остановлен после чтения состояния. Второй либо ждёт
// транзакцию, либо уже успел прочитать старое состояние и дойти до записи.
func overlapWrites(t *testing.T, s *Store, at time.Time, first, second func() error) {
	t.Helper()
	reached := make(chan struct{}, 2)
	release := make(chan struct{})
	s.SetClock(func() time.Time {
		reached <- struct{}{}
		<-release
		return at
	})
	errs := make(chan error, 2)
	waitCount := s.db.Stats().WaitCount
	go func() { errs <- first() }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("первый запрос не дошёл до записи")
	}
	go func() { errs <- second() }()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
wait:
	for s.db.Stats().WaitCount == waitCount {
		select {
		case <-reached:
			break wait
		case <-tick.C:
		case <-deadline:
			close(release)
			t.Fatal("второй запрос не начал запись")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestConcurrentDayEdits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	days := s.For(OwnerID)
	at := lt("2026-11-18 07:00")
	day, err := days.Wake(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	overlapWrites(t, s, at, func() error {
		_, err := days.SetField(ctx, day.ID, "report", "Запись")
		return err
	}, func() error {
		_, err := days.SetField(ctx, day.ID, "energy", "4")
		return err
	})
	got, err := days.Get(ctx, day.ID)
	if err != nil || got.Report != "Запись" || got.Energy == nil || *got.Energy != 4 {
		t.Fatalf("одна из записей потеряна: %+v %v", got, err)
	}
	overlapWrites(t, s, at, func() error {
		return days.Bed(ctx, day.ID, at.Add(10*time.Hour))
	}, func() error {
		_, err := days.SetFields(ctx, day.ID, map[string]string{"train_type": "Тренировка"})
		return err
	})
	if err := days.Bed(ctx, day.ID, at.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = days.Get(ctx, day.ID)
	if err != nil || got.BedAt == nil || !got.BedAt.Equal(at.Add(10*time.Hour)) || got.TrainType != "Тренировка" {
		t.Fatalf("отбой перезаписан или поле потеряно: %+v %v", got, err)
	}
}

func TestConcurrentWake(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	days := s.For(OwnerID)
	at := lt("2026-11-18 07:00")
	wake := func() error { _, err := days.Wake(ctx, at); return err }
	overlapWrites(t, s, at, wake, wake)
	if n, err := days.Count(ctx); err != nil || n != 1 {
		t.Fatalf("двойной подъём: %d записей, %v", n, err)
	}
}

func TestNavigationAtEqualWakeTime(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	days := s.For(OwnerID)
	at := lt("2026-11-18 07:00")
	a, err := days.Wake(ctx, at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := days.Wake(ctx, at.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	b, err = days.SetField(ctx, b.ID, "wake_at", at.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if prev, err := days.Prev(ctx, b); err != nil || prev == nil || prev.ID != a.ID {
		t.Fatalf("предыдущий день: %+v %v", prev, err)
	}
	if next, err := days.Next(ctx, a); err != nil || next == nil || next.ID != b.ID {
		t.Fatalf("следующий день: %+v %v", next, err)
	}
	if _, err := days.Import(ctx, []ImportRow{{Line: 2, WakeAt: at, Values: map[string]string{"report": "Импорт"}}}); err != nil {
		t.Fatal(err)
	}
	entries, err := days.Range(ctx, zeroT, zeroT)
	if err != nil || len(entries) != 2 || entries[0].ID != a.ID || entries[0].Report != "Импорт" || entries[1].Report != "" {
		t.Fatalf("импорт должен выбрать ранний день с меньшим id: %+v %v", entries, err)
	}
}

func TestConcurrentBackupPublication(t *testing.T) {
	db := filepath.Join(t.TempDir(), "data.db")
	a, err := Open(db, tz)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(db, tz)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "test.db")
	type result struct {
		path string
		err  error
	}
	results := make(chan result, 2)
	for _, st := range []*Store{a, b} {
		go func() {
			path, err := st.createDailyBackup(context.Background(), target)
			results <- result{path, err}
		}()
	}
	created := 0
	for range 2 {
		res := <-results
		if res.err != nil {
			t.Error(res.err)
		}
		if res.path == target {
			created++
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || created != 1 || len(files) != 1 || files[0].Name() != "test.db" {
		t.Fatalf("дневная копия: создано %d, файлов %d, ошибка %v", created, len(files), err)
	}
}
