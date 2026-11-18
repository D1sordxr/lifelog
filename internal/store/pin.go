package store

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PINLength — длина PIN, как у экрана блокировки iPhone.
const PINLength = 6

// Сколько попыток подряд без задержки; дальше блокировка растёт: 1, 2, 4 … мин, но не больше часа.
const (
	PINFreeAttempts = 5
	pinMaxLock      = time.Hour
)

// PINIterations — итерации PBKDF2-SHA256 (рекомендация OWASP). В тестах уменьшается.
var PINIterations = 600_000

var (
	ErrPINFormat = errors.New("PIN — ровно 6 цифр")
	ErrPINSet    = errors.New("PIN уже задан")
	ErrPINNotSet = errors.New("PIN не задан")
)

// PINLockedError — слишком много неверных попыток.
type PINLockedError struct{ Until time.Time }

func (e *PINLockedError) Error() string {
	return fmt.Sprintf("слишком много попыток, подожди до %s", e.Until.Format(time.RFC3339))
}

// PINStatus — состояние PIN пользователя.
type PINStatus struct {
	Set         bool
	Failed      int
	LockedUntil time.Time
}

func validPIN(pin string) bool {
	if len(pin) != PINLength {
		return false
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// HashPIN → "pbkdf2-sha256$итерации$соль$хэш" (base64).
func HashPIN(pin string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pin, salt, PINIterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", PINIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func verifyPIN(pin, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err != nil || err1 != nil || err2 != nil || iter < 1 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pin, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// PINStatus возвращает состояние PIN пользователя.
func (s *Store) PINStatus(ctx context.Context, uid int64) (PINStatus, error) {
	var (
		hash   sql.NullString
		failed int
		locked nullTime
	)
	err := s.db.QueryRowContext(ctx, `SELECT pin_hash, pin_failed, pin_locked_until FROM users WHERE id = ?`, uid).Scan(&hash, &failed, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return PINStatus{}, ErrUserNotFound
	}
	if err != nil {
		return PINStatus{}, err
	}
	st := PINStatus{Set: hash.String != "", Failed: failed}
	if locked.t != nil {
		st.LockedUntil = *locked.t
	}
	return st, nil
}

// SetPIN задаёт PIN, только если он ещё не задан (сменить через интерфейс нельзя —
// иначе захваченная сессия могла бы переписать PIN). Сброс — ResetPIN на сервере.
func (s *Store) SetPIN(ctx context.Context, uid int64, pin string) error {
	if !validPIN(pin) {
		return ErrPINFormat
	}
	hash, err := HashPIN(pin)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET pin_hash = ?, pin_failed = 0, pin_locked_until = NULL
		WHERE id = ? AND (pin_hash IS NULL OR pin_hash = '')`, hash, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPINSet
	}
	return nil
}

// CheckPIN проверяет PIN. Попытка засчитывается (и при необходимости ставится
// блокировка) ДО проверки, в отдельной транзакции, — параллельные запросы не дают
// лишних попыток. Верный PIN сбрасывает счётчик. Во время блокировки PIN не
// проверяется вовсе, даже верный.
// Возвращает: ok, сколько попыток осталось до блокировки, ошибку
// (*PINLockedError при блокировке).
func (s *Store) CheckPIN(ctx context.Context, uid int64, pin string) (bool, int, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, err
	}
	var (
		hash   sql.NullString
		failed int
		locked nullTime
	)
	err = tx.QueryRowContext(ctx, `SELECT pin_hash, pin_failed, pin_locked_until FROM users WHERE id = ?`, uid).Scan(&hash, &failed, &locked)
	if err != nil {
		tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return false, 0, ErrUserNotFound
		}
		return false, 0, err
	}
	if hash.String == "" {
		tx.Rollback()
		return false, 0, ErrPINNotSet
	}
	if locked.t != nil && now.Before(*locked.t) {
		tx.Rollback()
		return false, 0, &PINLockedError{Until: *locked.t}
	}
	failed++
	var lockUntil any
	if failed >= PINFreeAttempts {
		d := time.Minute << min(failed-PINFreeAttempts, 10)
		lockUntil = fmtTime(now.Add(min(d, pinMaxLock)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET pin_failed = ?, pin_locked_until = ? WHERE id = ?`, failed, lockUntil, uid); err != nil {
		tx.Rollback()
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, err
	}

	if validPIN(pin) && verifyPIN(pin, hash.String) {
		_, err := s.db.ExecContext(ctx, `UPDATE users SET pin_failed = 0, pin_locked_until = NULL WHERE id = ?`, uid)
		return err == nil, 0, err
	}
	return false, max(PINFreeAttempts-failed, 0), nil
}

// ResetPIN стирает PIN пользователя с Telegram id tgID (команда lifelog reset-pin).
func (s *Store) ResetPIN(ctx context.Context, tgID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET pin_hash = NULL, pin_failed = 0, pin_locked_until = NULL WHERE tg_id = ?`, tgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

// ErrPINSame — новый PIN совпадает с текущим.
var ErrPINSame = errors.New("новый PIN совпадает с текущим")

// ChangePIN меняет PIN, если верен текущий. Проверка текущего — та же CheckPIN:
// неверный ввод засчитывается как неудачная попытка и ведёт к блокировке,
// так что подобрать PIN через смену не получится.
// Возвращает ok и сколько попыток осталось до блокировки.
func (s *Store) ChangePIN(ctx context.Context, uid int64, oldPIN, newPIN string) (bool, int, error) {
	if !validPIN(newPIN) {
		return false, 0, ErrPINFormat
	}
	ok, left, err := s.CheckPIN(ctx, uid, oldPIN)
	if err != nil || !ok {
		return false, left, err
	}
	if oldPIN == newPIN {
		return false, 0, ErrPINSame
	}
	hash, err := HashPIN(newPIN)
	if err != nil {
		return false, 0, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET pin_hash = ?, pin_failed = 0, pin_locked_until = NULL WHERE id = ?`, hash, uid); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}
