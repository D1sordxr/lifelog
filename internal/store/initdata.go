package store

import (
	"context"
	"time"
)

// UseInitData помечает initData (по его hash) использованным. Возвращает false,
// если он уже был использован — повтор. Записи старше expires удаляются: после
// этого срока initData отвергается по auth_date и без таблицы.
func (s *Store) UseInitData(ctx context.Context, hash string, expires time.Time) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM used_init_data WHERE expires_at < ?`, fmtTime(s.now())); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO used_init_data (hash, expires_at) VALUES (?, ?)`, hash, fmtTime(expires))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
