package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// OwnerID — пользователь, которому принадлежат дни, записанные без входа
// (локальный режим без Telegram и данные до миграции 0002).
const OwnerID int64 = 1

// User — пользователь журнала.
type User struct {
	ID        int64
	TGID      int64 // 0 — ещё не привязан к Telegram
	Username  string
	CreatedAt time.Time
}

func scanUser(sc scanner) (*User, error) {
	var (
		u       User
		tgID    sql.NullInt64
		name    sql.NullString
		created nullTime
	)
	if err := sc.Scan(&u.ID, &tgID, &name, &created); err != nil {
		return nil, err
	}
	u.TGID = tgID.Int64
	u.Username = name.String
	if created.t != nil {
		u.CreatedAt = *created.t
	}
	return &u, nil
}

// UserByID — пользователь по id или ErrUserNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT id, tg_id, username, created_at FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return u, err
}

// ErrUserNotFound — нет такого пользователя.
var ErrUserNotFound = errors.New("пользователь не найден")

// TelegramUser находит или создаёт пользователя по Telegram id и обновляет username.
// claimOwner: если пользователя с таким tg_id ещё нет, он занимает строку OwnerID,
// пока она не привязана к Telegram (так владелец получает свои старые записи).
func (s *Store) TelegramUser(ctx context.Context, tgID int64, username string, claimOwner bool) (*User, error) {
	username = strings.TrimPrefix(username, "@")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT id, tg_id, username, created_at FROM users WHERE tg_id = ?`, tgID))
	switch {
	case err == nil:
		if u.Username != username {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET username = ? WHERE id = ?`, nullStr(username), u.ID); err != nil {
				return nil, err
			}
			u.Username = username
		}
	case errors.Is(err, sql.ErrNoRows):
		claimed := false
		if claimOwner {
			res, err := tx.ExecContext(ctx, `UPDATE users SET tg_id = ?, username = ? WHERE id = ? AND tg_id IS NULL`, tgID, nullStr(username), OwnerID)
			if err != nil {
				return nil, err
			}
			n, _ := res.RowsAffected()
			claimed = n == 1
		}
		if !claimed {
			if _, err := tx.ExecContext(ctx, `INSERT INTO users (tg_id, username, created_at) VALUES (?, ?, ?)`,
				tgID, nullStr(username), fmtTime(s.now())); err != nil {
				return nil, err
			}
		}
		u, err = scanUser(tx.QueryRowContext(ctx, `SELECT id, tg_id, username, created_at FROM users WHERE tg_id = ?`, tgID))
		if err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return u, tx.Commit()
}
