package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Day — одни сутки бодрствования: от подъёма (WakeAt) до отбоя (BedAt).
// Все времена в UTC; «дата дня» — локальная дата подъёма.
type Day struct {
	ID           int64
	WakeAt       time.Time
	BedAt        *time.Time
	TrainStart   *time.Time
	TrainMinutes *int
	TrainType    string
	Energy       *int
	Report       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Entry — день вместе с вычисленной длительностью сна перед ним.
type Entry struct {
	Day
	Sleep *time.Duration // nil, если сон неизвестен
}

// ErrNotFound — дня с таким id нет.
var ErrNotFound = errors.New("день не найден")

// MaxSleep — интервал «отбой → подъём» длиннее этого считается пропуском
// (не отмечены дни), а не сном.
const MaxSleep = 20 * time.Hour

// StaleAfter — если текущий день открыт дольше, скорее всего забыли нажать «Лёг».
const StaleAfter = 20 * time.Hour

// SleepBetween — сон перед днём d: d.WakeAt − prev.BedAt.
// nil, если предыдущего дня нет, у него нет отбоя, интервал неположителен
// или длиннее MaxSleep (пропущенные дни).
func SleepBetween(prev *Day, d *Day) *time.Duration {
	if prev == nil || prev.BedAt == nil {
		return nil
	}
	dur := d.WakeAt.Sub(*prev.BedAt)
	if dur <= 0 || dur > MaxSleep {
		return nil
	}
	return &dur
}

// WithSleep вычисляет сон для дней, отсортированных по WakeAt по возрастанию.
// prev — день, предшествующий первому из days (или nil).
func WithSleep(prev *Day, days []Day) []Entry {
	out := make([]Entry, len(days))
	for i := range days {
		out[i] = Entry{Day: days[i], Sleep: SleepBetween(prev, &days[i])}
		prev = &days[i]
	}
	return out
}

// LocalDate — дата дня (локальная дата подъёма) в формате 2006-01-02.
func (d *Day) LocalDate(loc *time.Location) string { return d.WakeAt.In(loc).Format("2006-01-02") }

const dayCols = `id, wake_at, bed_at, train_start, train_minutes, train_type, energy, report, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

// querier — общее у *sql.DB и *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanDay(sc scanner) (Day, error) {
	var (
		d                                  Day
		wake, bed, train, created, updated nullTime
		trainMin, energy                   sql.NullInt64
		trainType, report                  sql.NullString
	)
	if err := sc.Scan(&d.ID, &wake, &bed, &train, &trainMin, &trainType, &energy, &report, &created, &updated); err != nil {
		return d, err
	}
	if wake.t == nil {
		return d, fmt.Errorf("day %d: wake_at is NULL", d.ID)
	}
	d.WakeAt = *wake.t
	d.BedAt = bed.t
	d.TrainStart = train.t
	if trainMin.Valid {
		v := int(trainMin.Int64)
		d.TrainMinutes = &v
	}
	if energy.Valid {
		v := int(energy.Int64)
		d.Energy = &v
	}
	d.TrainType = trainType.String
	d.Report = report.String
	if created.t != nil {
		d.CreatedAt = *created.t
	}
	if updated.t != nil {
		d.UpdatedAt = *updated.t
	}
	return d, nil
}

func queryDays(ctx context.Context, q querier, query string, args ...any) ([]Day, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Day
	for rows.Next() {
		d, err := scanDay(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func queryDay(ctx context.Context, q querier, query string, args ...any) (*Day, error) {
	d, err := scanDay(q.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func insertDay(ctx context.Context, q querier, d *Day, now time.Time) error {
	res, err := q.ExecContext(ctx, `INSERT INTO days
		(wake_at, bed_at, train_start, train_minutes, train_type, energy, report, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fmtTime(d.WakeAt), fmtTimePtr(d.BedAt), fmtTimePtr(d.TrainStart), nullInt(d.TrainMinutes),
		nullStr(d.TrainType), nullInt(d.Energy), nullStr(d.Report), fmtTime(now), fmtTime(now))
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	d.CreatedAt, d.UpdatedAt = now.UTC(), now.UTC()
	return err
}

func updateDay(ctx context.Context, q querier, d *Day, now time.Time) error {
	res, err := q.ExecContext(ctx, `UPDATE days SET
		wake_at = ?, bed_at = ?, train_start = ?, train_minutes = ?, train_type = ?,
		energy = ?, report = ?, updated_at = ?
		WHERE id = ?`,
		fmtTime(d.WakeAt), fmtTimePtr(d.BedAt), fmtTimePtr(d.TrainStart), nullInt(d.TrainMinutes),
		nullStr(d.TrainType), nullInt(d.Energy), nullStr(d.Report), fmtTime(now), d.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	d.UpdatedAt = now.UTC()
	return nil
}

// --- публичные операции ---

// Get возвращает день по id или ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (*Day, error) {
	d, err := queryDay(ctx, s.db, `SELECT `+dayCols+` FROM days WHERE id = ?`, id)
	if err == nil && d == nil {
		err = ErrNotFound
	}
	return d, err
}

// Latest — самый поздний по подъёму день (nil, если дней нет).
func (s *Store) Latest(ctx context.Context) (*Day, error) {
	return queryDay(ctx, s.db, `SELECT `+dayCols+` FROM days ORDER BY wake_at DESC, id DESC LIMIT 1`)
}

// Current — «текущий день»: последний день, если у него ещё нет отбоя; иначе nil.
// Более старые дни без отбоя (забыли нажать «Лёг») текущими не считаются.
func (s *Store) Current(ctx context.Context) (*Day, error) {
	d, err := s.Latest(ctx)
	if err != nil || d == nil || d.BedAt != nil {
		return nil, err
	}
	return d, nil
}

// Prev — день, предшествующий моменту подъёма t.
func (s *Store) Prev(ctx context.Context, t time.Time) (*Day, error) {
	return queryDay(ctx, s.db, `SELECT `+dayCols+` FROM days WHERE wake_at < ? ORDER BY wake_at DESC, id DESC LIMIT 1`, fmtTime(t))
}

// Next — следующий день после момента подъёма t.
func (s *Store) Next(ctx context.Context, t time.Time) (*Day, error) {
	return queryDay(ctx, s.db, `SELECT `+dayCols+` FROM days WHERE wake_at > ? ORDER BY wake_at ASC, id ASC LIMIT 1`, fmtTime(t))
}

// Wake создаёт новый день с подъёмом в момент t.
// Защита от двойного нажатия: если последний день открыт и начат меньше
// 2 минут назад, возвращается он.
func (s *Store) Wake(ctx context.Context, t time.Time) (*Day, error) {
	if last, err := s.Latest(ctx); err != nil {
		return nil, err
	} else if last != nil && last.BedAt == nil && t.Sub(last.WakeAt) >= 0 && t.Sub(last.WakeAt) < 2*time.Minute {
		return last, nil
	}
	d := &Day{WakeAt: t.UTC().Truncate(time.Second)}
	if err := insertDay(ctx, s.db, d, s.now()); err != nil {
		return nil, err
	}
	return d, nil
}

// Save записывает все поля дня.
func (s *Store) Save(ctx context.Context, d *Day) error {
	if err := d.Validate(); err != nil {
		return &InputError{err}
	}
	return updateDay(ctx, s.db, d, s.now())
}

// Delete удаляет день.
func (s *Store) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM days WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Page — страница истории (новые сверху) с вычисленным сном.
func (s *Store) Page(ctx context.Context, limit, offset int) ([]Entry, error) {
	desc, err := queryDays(ctx, s.db, `SELECT `+dayCols+` FROM days ORDER BY wake_at DESC, id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil || len(desc) == 0 {
		return nil, err
	}
	asc := make([]Day, len(desc))
	for i := range desc {
		asc[len(desc)-1-i] = desc[i]
	}
	prev, err := s.Prev(ctx, asc[0].WakeAt)
	if err != nil {
		return nil, err
	}
	entries := WithSleep(prev, asc)
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// Count — сколько всего дней.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM days`).Scan(&n)
	return n, err
}

// Range — дни с подъёмом в [from, to) по возрастанию, с вычисленным сном.
// Нулевые from/to означают «без границы». Для первого дня сон считается
// по дню, предшествующему диапазону.
func (s *Store) Range(ctx context.Context, from, to time.Time) ([]Entry, error) {
	q := `SELECT ` + dayCols + ` FROM days WHERE 1=1`
	var args []any
	if !from.IsZero() {
		q += ` AND wake_at >= ?`
		args = append(args, fmtTime(from))
	}
	if !to.IsZero() {
		q += ` AND wake_at < ?`
		args = append(args, fmtTime(to))
	}
	q += ` ORDER BY wake_at ASC, id ASC`
	days, err := queryDays(ctx, s.db, q, args...)
	if err != nil || len(days) == 0 {
		return nil, err
	}
	prev, err := s.Prev(ctx, days[0].WakeAt)
	if err != nil {
		return nil, err
	}
	return WithSleep(prev, days), nil
}

// SleepBefore — сон перед днём d.
func (s *Store) SleepBefore(ctx context.Context, d *Day) (*time.Duration, error) {
	prev, err := s.Prev(ctx, d.WakeAt)
	if err != nil {
		return nil, err
	}
	return SleepBetween(prev, d), nil
}

// TrainTypes — ранее использованные типы тренировок, недавние первыми.
func (s *Store) TrainTypes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT train_type FROM days
		WHERE train_type IS NOT NULL AND train_type <> ''
		GROUP BY train_type ORDER BY MAX(wake_at) DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DayBounds — UTC-границы локальной даты date (2006-01-02): [начало, начало следующей).
func DayBounds(date string, loc *time.Location) (time.Time, time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return t, t.AddDate(0, 0, 1), nil
}
