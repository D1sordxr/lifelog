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

func insertDay(ctx context.Context, q querier, uid int64, d *Day, now time.Time) error {
	res, err := q.ExecContext(ctx, `INSERT INTO days
		(user_id, wake_at, bed_at, train_start, train_minutes, train_type, energy, report, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, fmtTime(d.WakeAt), fmtTimePtr(d.BedAt), fmtTimePtr(d.TrainStart), nullInt(d.TrainMinutes),
		nullStr(d.TrainType), nullInt(d.Energy), nullStr(d.Report), fmtTime(now), fmtTime(now))
	if err != nil {
		return err
	}
	d.ID, err = res.LastInsertId()
	d.CreatedAt, d.UpdatedAt = now.UTC(), now.UTC()
	return err
}

func updateDay(ctx context.Context, q querier, uid int64, d *Day, now time.Time) error {
	res, err := q.ExecContext(ctx, `UPDATE days SET
		wake_at = ?, bed_at = ?, train_start = ?, train_minutes = ?, train_type = ?,
		energy = ?, report = ?, updated_at = ?
		WHERE id = ? AND user_id = ?`,
		fmtTime(d.WakeAt), fmtTimePtr(d.BedAt), fmtTimePtr(d.TrainStart), nullInt(d.TrainMinutes),
		nullStr(d.TrainType), nullInt(d.Energy), nullStr(d.Report), fmtTime(now), d.ID, uid)
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

// Days — дни одного пользователя. Все запросы ограничены его user_id:
// чужой день по id не найти и не изменить.
type Days struct {
	s   *Store
	uid int64
}

// For возвращает дни пользователя uid.
func (s *Store) For(uid int64) *Days { return &Days{s: s, uid: uid} }

// UserID — чьи это дни.
func (d *Days) UserID() int64 { return d.uid }

// Get возвращает день по id или ErrNotFound.
func (d *Days) Get(ctx context.Context, id int64) (*Day, error) {
	return d.get(ctx, d.s.db, id)
}

func (d *Days) get(ctx context.Context, q querier, id int64) (*Day, error) {
	day, err := queryDay(ctx, q, `SELECT `+dayCols+` FROM days WHERE id = ? AND user_id = ?`, id, d.uid)
	if err == nil && day == nil {
		err = ErrNotFound
	}
	return day, err
}

// Latest — самый поздний по подъёму день (nil, если дней нет).
func (d *Days) Latest(ctx context.Context) (*Day, error) {
	return d.latest(ctx, d.s.db)
}

func (d *Days) latest(ctx context.Context, q querier) (*Day, error) {
	return queryDay(ctx, q, `SELECT `+dayCols+` FROM days WHERE user_id = ? ORDER BY wake_at DESC, id DESC LIMIT 1`, d.uid)
}

// Current — «текущий день»: последний день, если у него ещё нет отбоя; иначе nil.
// Более старые дни без отбоя (забыли нажать «Лёг») текущими не считаются.
func (d *Days) Current(ctx context.Context) (*Day, error) {
	day, err := d.Latest(ctx)
	if err != nil || day == nil || day.BedAt != nil {
		return nil, err
	}
	return day, nil
}

// Prev — предыдущий день в порядке подъёма, при равенстве — по id.
func (d *Days) Prev(ctx context.Context, day *Day) (*Day, error) {
	return queryDay(ctx, d.s.db, `SELECT `+dayCols+` FROM days WHERE user_id = ? AND (wake_at, id) < (?, ?) ORDER BY wake_at DESC, id DESC LIMIT 1`, d.uid, fmtTime(day.WakeAt), day.ID)
}

// Next — следующий день в том же порядке.
func (d *Days) Next(ctx context.Context, day *Day) (*Day, error) {
	return queryDay(ctx, d.s.db, `SELECT `+dayCols+` FROM days WHERE user_id = ? AND (wake_at, id) > (?, ?) ORDER BY wake_at ASC, id ASC LIMIT 1`, d.uid, fmtTime(day.WakeAt), day.ID)
}

// Wake создаёт новый день с подъёмом в момент t.
// Повторное нажатие в течение двух минут возвращает уже открытый день.
func (d *Days) Wake(ctx context.Context, t time.Time) (*Day, error) {
	t = t.UTC().Truncate(time.Second)
	tx, err := d.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	last, err := d.latest(ctx, tx)
	if err != nil {
		return nil, err
	}
	if last != nil && last.BedAt == nil {
		// При двойном нажатии второй запрос может первым получить транзакцию.
		delta := t.Sub(last.WakeAt)
		if delta > -2*time.Minute && delta < 2*time.Minute {
			return last, nil
		}
	}
	day := &Day{WakeAt: t}
	if err := insertDay(ctx, tx, d.uid, day, d.s.now()); err != nil {
		return nil, err
	}
	return day, tx.Commit()
}

// Bed отмечает отбой открытого дня, сохраняя время первого запроса.
func (d *Days) Bed(ctx context.Context, id int64, t time.Time) error {
	tx, err := d.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	day, err := d.get(ctx, tx, id)
	if err != nil || day.BedAt != nil {
		return err
	}
	t = t.UTC().Truncate(time.Second)
	day.BedAt = &t
	if err := day.Validate(); err != nil {
		return &InputError{err}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE days SET bed_at = ?, updated_at = ? WHERE id = ? AND user_id = ? AND bed_at IS NULL`,
		fmtTime(t), fmtTime(d.s.now()), id, d.uid); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete удаляет день.
func (d *Days) Delete(ctx context.Context, id int64) error {
	res, err := d.s.db.ExecContext(ctx, `DELETE FROM days WHERE id = ? AND user_id = ?`, id, d.uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Page — страница истории (новые сверху) с вычисленным сном.
func (d *Days) Page(ctx context.Context, limit, offset int) ([]Entry, error) {
	desc, err := queryDays(ctx, d.s.db, `SELECT `+dayCols+` FROM days WHERE user_id = ? ORDER BY wake_at DESC, id DESC LIMIT ? OFFSET ?`, d.uid, limit, offset)
	if err != nil || len(desc) == 0 {
		return nil, err
	}
	asc := make([]Day, len(desc))
	for i := range desc {
		asc[len(desc)-1-i] = desc[i]
	}
	prev, err := d.Prev(ctx, &asc[0])
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
func (d *Days) Count(ctx context.Context) (int, error) {
	var n int
	err := d.s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM days WHERE user_id = ?`, d.uid).Scan(&n)
	return n, err
}

// Range — дни с подъёмом в [from, to) по возрастанию, с вычисленным сном.
// Нулевые from/to означают «без границы». Для первого дня сон считается
// по дню, предшествующему диапазону.
func (d *Days) Range(ctx context.Context, from, to time.Time) ([]Entry, error) {
	q := `SELECT ` + dayCols + ` FROM days WHERE user_id = ?`
	args := []any{d.uid}
	if !from.IsZero() {
		q += ` AND wake_at >= ?`
		args = append(args, fmtTime(from))
	}
	if !to.IsZero() {
		q += ` AND wake_at < ?`
		args = append(args, fmtTime(to))
	}
	q += ` ORDER BY wake_at ASC, id ASC`
	days, err := queryDays(ctx, d.s.db, q, args...)
	if err != nil || len(days) == 0 {
		return nil, err
	}
	prev, err := d.Prev(ctx, &days[0])
	if err != nil {
		return nil, err
	}
	return WithSleep(prev, days), nil
}

// SleepBefore — сон перед днём day.
func (d *Days) SleepBefore(ctx context.Context, day *Day) (*time.Duration, error) {
	prev, err := d.Prev(ctx, day)
	if err != nil {
		return nil, err
	}
	return SleepBetween(prev, day), nil
}

// TrainTypes — ранее использованные типы тренировок, недавние первыми.
func (d *Days) TrainTypes(ctx context.Context) ([]string, error) {
	rows, err := d.s.db.QueryContext(ctx, `SELECT train_type FROM days
		WHERE user_id = ? AND train_type IS NOT NULL AND train_type <> ''
		GROUP BY train_type ORDER BY MAX(wake_at) DESC LIMIT 30`, d.uid)
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
