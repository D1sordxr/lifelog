package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestUsersIsolation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	owner, err := s.TelegramUser(ctx, 111, "@Owner", true)
	if err != nil || owner.ID != OwnerID || owner.Username != "Owner" {
		t.Fatalf("owner claims row 1: %+v %v", owner, err)
	}
	tester, err := s.TelegramUser(ctx, 222, "tester", true) // строка 1 уже занята
	if err != nil || tester.ID == OwnerID {
		t.Fatalf("tester: %+v %v", tester, err)
	}
	again, _ := s.TelegramUser(ctx, 222, "renamed", false)
	if again.ID != tester.ID || again.Username != "renamed" {
		t.Fatalf("relogin: %+v", again)
	}

	od, td := s.For(owner.ID), s.For(tester.ID)
	day, _ := od.Wake(ctx, lt("2026-11-18 07:00"))
	if cur, _ := td.Current(ctx); cur != nil {
		t.Fatal("tester sees owner's current day")
	}
	if _, err := td.Get(ctx, day.ID); err != ErrNotFound {
		t.Fatalf("tester Get owner's day: %v", err)
	}
	if _, err := td.SetField(ctx, day.ID, "report", "взлом"); err != ErrNotFound {
		t.Fatalf("tester SetField owner's day: %v", err)
	}
	if err := td.Delete(ctx, day.ID); err != ErrNotFound {
		t.Fatalf("tester Delete owner's day: %v", err)
	}
	if n, _ := td.Count(ctx); n != 0 {
		t.Fatalf("tester count %d", n)
	}
	td.Wake(ctx, lt("2026-11-18 09:00"))
	if e, _ := od.Range(ctx, zeroT, zeroT); len(e) != 1 || e[0].ID != day.ID {
		t.Fatalf("owner range: %+v", e)
	}
}

func TestMigrationKeepsOldDaysForOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := migrationsFS.ReadFile("migrations/0001_init.sql")
	for _, q := range []string{
		`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		string(v1),
		`INSERT INTO schema_migrations VALUES (1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO days (wake_at, bed_at, report, created_at, updated_at) VALUES
			('2026-11-17T03:00:00Z', '2026-11-17T19:30:00Z', 'старый день', '2026-11-17T03:00:00Z', '2026-11-17T03:00:00Z')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	s, err := Open(path, tz)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	e, err := s.For(OwnerID).Range(ctx, zeroT, zeroT)
	if err != nil || len(e) != 1 || e[0].Report != "старый день" {
		t.Fatalf("old day after migration: %+v %v", e, err)
	}
	u, _ := s.TelegramUser(ctx, 111, "me", true)
	if n, _ := s.For(u.ID).Count(ctx); n != 1 {
		t.Fatal("owner did not get old days")
	}
}

func TestConcurrentOpenMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			s, err := Open(path, tz)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
}

func TestUseInitData(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := lt("2026-11-18 10:00")
	s.SetClock(func() time.Time { return now })
	if ok, err := s.UseInitData(ctx, "abc", now.Add(15*time.Minute)); !ok || err != nil {
		t.Fatalf("first: %v %v", ok, err)
	}
	if ok, _ := s.UseInitData(ctx, "abc", now.Add(15*time.Minute)); ok {
		t.Fatal("replay accepted")
	}
	// просроченные записи чистятся
	now = now.Add(time.Hour)
	s.UseInitData(ctx, "other", now.Add(15*time.Minute))
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM used_init_data`).Scan(&n)
	if n != 1 {
		t.Fatalf("cleanup: %d rows", n)
	}
}
