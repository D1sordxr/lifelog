package store

import (
	"context"
	"fmt"
	"time"
)

// ImportRow — одна строка импорта. Values — непустые значения полей реестра
// (времена уже полные, см. export.ParseCSV); WakeAt — момент подъёма.
type ImportRow struct {
	Line   int
	WakeAt time.Time
	Values map[string]string
}

// ImportResult — итог импорта.
type ImportResult struct {
	Added, Updated int
	Errors         []string
}

// Import применяет строки в одной транзакции. День сопоставляется по дате
// подъёма (локальной): если такой уже есть — обновляются только поля, непустые
// в строке; иначе создаётся новый. При любой ошибке не применяется ничего.
func (s *Store) Import(ctx context.Context, rows []ImportRow) (ImportResult, error) {
	var res ImportResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	now := s.now()

	for _, r := range rows {
		from, to, _ := DayBounds(r.WakeAt.In(s.loc).Format("2006-01-02"), s.loc)
		d, err := queryDay(ctx, tx, `SELECT `+dayCols+` FROM days WHERE wake_at >= ? AND wake_at < ? ORDER BY wake_at, id LIMIT 1`,
			fmtTime(from), fmtTime(to))
		if err != nil {
			return res, err
		}
		isNew := d == nil
		if isNew {
			d = &Day{}
		}
		d.WakeAt = r.WakeAt.UTC()
		rowErr := func() error {
			for _, f := range Fields {
				if f.Name == "wake_at" {
					continue
				}
				if v, ok := r.Values[f.Name]; ok && v != "" {
					if err := f.Set(d, v, s.loc, now); err != nil {
						return fmt.Errorf("%s: %w", f.Name, err)
					}
				}
			}
			return d.Validate()
		}()
		if rowErr != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("строка %d: %v", r.Line, rowErr))
			continue
		}
		if isNew {
			err = insertDay(ctx, tx, d, now)
			res.Added++
		} else {
			err = updateDay(ctx, tx, d, now)
			res.Updated++
		}
		if err != nil {
			return res, fmt.Errorf("строка %d: %w", r.Line, err)
		}
	}
	if len(res.Errors) > 0 {
		res.Added, res.Updated = 0, 0
		return res, nil
	}
	return res, tx.Commit()
}
