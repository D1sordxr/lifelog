// Package store хранит дни в SQLite и содержит доменную логику (сон, текущий день).
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store — доступ к базе. Безопасен для конкурентного использования.
type Store struct {
	db  *sql.DB
	loc *time.Location
	now func() time.Time
}

// Open открывает (или создаёт) базу, включает WAL и применяет миграции.
// loc — часовой пояс, в котором определяется «дата дня» и разбирается ввод.
func Open(path string, loc *time.Location) (*Store, error) {
	// Параметры после '?' modernc.org/sqlite срезает с пути и применяет как PRAGMA.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Один пользователь, нагрузка мизерная: одно соединение исключает
	// любые SQLITE_BUSY между запросами самого приложения.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, loc: loc, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Loc — часовой пояс отображения.
func (s *Store) Loc() *time.Location { return s.loc }

// SetClock подменяет часы (для тестов).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

type migration struct {
	version int
	name    string
}

// migrate применяет по порядку все migrations/NNNN_*.sql, которых ещё нет в schema_migrations.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	var migs []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok || !strings.HasSuffix(e.Name(), ".sql") {
			return fmt.Errorf("bad migration name %q (want NNNN_name.sql)", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("bad migration name %q: %w", e.Name(), err)
		}
		migs = append(migs, migration{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	applied := map[int]bool{}
	rows, err := s.db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, fmtTime(s.now())); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

const timeLayout = "2006-01-02T15:04:05Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// nullTime сканирует DATETIME-колонку: драйвер может отдать как строку, так и time.Time.
type nullTime struct{ t *time.Time }

func (n *nullTime) Scan(src any) error {
	n.t = nil
	var str string
	switch v := src.(type) {
	case nil:
		return nil
	case time.Time:
		t := v.UTC()
		n.t = &t
		return nil
	case string:
		str = v
	case []byte:
		str = string(v)
	default:
		return fmt.Errorf("unexpected time type %T", src)
	}
	for _, layout := range []string{timeLayout, time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, str); err == nil {
			t = t.UTC()
			n.t = &t
			return nil
		}
	}
	return fmt.Errorf("cannot parse time %q", str)
}
