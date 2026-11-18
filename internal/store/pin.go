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
	ErrPINFormat  = errors.New("PIN — ровно 6 цифр")
	ErrPINSet     = errors.New("PIN уже задан")
	ErrPINNotSet  = errors.New("PIN не задан")
	ErrPINChanged = errors.New("PIN или блокировка изменились. Повтори вход")
)

// PINLockedError — слишком много неверных попыток.
type PINLockedError struct{ Until time.Time }

func (e *PINLockedError) Error() string {
	return fmt.Sprintf("слишком много попыток, подожди до %s", e.Until.Format(time.RFC3339))
}

// PINStatus — состояние PIN пользователя.
type PINStatus struct {
	Version     int64
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
		hash    sql.NullString
		failed  int
		locked  nullTime
		version int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT pin_hash, pin_failed, pin_locked_until, pin_version FROM users WHERE id = ?`, uid).Scan(&hash, &failed, &locked, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return PINStatus{}, ErrUserNotFound
	}
	if err != nil {
		return PINStatus{}, err
	}
	st := PINStatus{Version: version, Set: hash.String != "", Failed: failed}
	if locked.t != nil {
		st.LockedUntil = *locked.t
	}
	return st, nil
}

// SetPIN задаёт PIN, только если он ещё не задан, и возвращает версию разблокировки.
func (s *Store) SetPIN(ctx context.Context, uid int64, pin string) (int64, error) {
	if !validPIN(pin) {
		return 0, ErrPINFormat
	}
	hash, err := HashPIN(pin)
	if err != nil {
		return 0, err
	}
	var version int64
	err = s.db.QueryRowContext(ctx, `UPDATE users SET pin_hash = ?, pin_failed = 0, pin_locked_until = NULL
		WHERE id = ? AND (pin_hash IS NULL OR pin_hash = '') RETURNING pin_version`, hash, uid).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPINSet
	}
	return version, err
}

// PINCheck — результат проверки. Version используется для выдачи разблокировки при OK.
type PINCheck struct {
	OK      bool
	Left    int
	Version int64
}

type pinProof struct {
	hash    string
	version int64
}

// reservePINAttempt засчитывает попытку до PBKDF2, сериализуя проверки счётчика.
func (s *Store) reservePINAttempt(ctx context.Context, uid int64) (pinProof, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return pinProof{}, 0, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var (
		proof  pinProof
		failed int
		locked nullTime
	)
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(pin_hash, ''), pin_version, pin_failed, pin_locked_until
		FROM users WHERE id = ?`, uid).Scan(&proof.hash, &proof.version, &failed, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return pinProof{}, 0, ErrUserNotFound
	}
	if err != nil {
		return pinProof{}, 0, err
	}
	if proof.hash == "" {
		return pinProof{}, 0, ErrPINNotSet
	}
	if locked.t != nil && now.Before(*locked.t) {
		return pinProof{}, 0, &PINLockedError{Until: *locked.t}
	}
	failed++
	var lockUntil any
	if failed >= PINFreeAttempts {
		d := time.Minute << min(failed-PINFreeAttempts, 10)
		lockUntil = fmtTime(now.Add(min(d, pinMaxLock)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET pin_failed = ?, pin_locked_until = ? WHERE id = ?`, failed, lockUntil, uid); err != nil {
		return pinProof{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return pinProof{}, 0, err
	}
	return proof, max(PINFreeAttempts-failed, 0), nil
}

// finishPINCheck применяет успешную проверку только к тому состоянию, которое проверялось.
func (s *Store) finishPINCheck(ctx context.Context, uid int64, proof pinProof, newHash string) (PINCheck, error) {
	version := proof.version
	if newHash == "" {
		newHash = proof.hash
	} else {
		version++
	}
	result, err := s.db.ExecContext(ctx, `UPDATE users SET pin_hash = ?, pin_version = ?, pin_failed = 0, pin_locked_until = NULL
		WHERE id = ? AND pin_hash = ? AND pin_version = ?`, newHash, version, uid, proof.hash, proof.version)
	if err != nil {
		return PINCheck{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return PINCheck{}, err
	}
	if n == 0 {
		return PINCheck{}, ErrPINChanged
	}
	return PINCheck{OK: true, Version: version}, nil
}

// CheckPIN проверяет PIN и сбрасывает счётчик при успехе.
func (s *Store) CheckPIN(ctx context.Context, uid int64, pin string) (PINCheck, error) {
	proof, left, err := s.reservePINAttempt(ctx, uid)
	if err != nil {
		return PINCheck{}, err
	}
	if !validPIN(pin) || !verifyPIN(pin, proof.hash) {
		return PINCheck{Left: left}, nil
	}
	return s.finishPINCheck(ctx, uid, proof, "")
}

// LockPIN отзывает все разблокировки текущей версии PIN.
func (s *Store) LockPIN(ctx context.Context, uid, version int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET pin_version = pin_version + 1
		WHERE id = ? AND pin_version = ?`, uid, version)
	return err
}

// ResetPIN стирает PIN пользователя с Telegram id tgID (команда lifelog reset-pin).
func (s *Store) ResetPIN(ctx context.Context, tgID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET pin_hash = NULL, pin_failed = 0, pin_locked_until = NULL,
		pin_version = pin_version + 1 WHERE tg_id = ?`, tgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserNotFound
	}
	return nil
}

var ErrPINSame = errors.New("новый PIN совпадает с текущим")

// ChangePIN применяет новый PIN, только если проверенный хэш и версия ещё актуальны.
func (s *Store) ChangePIN(ctx context.Context, uid int64, oldPIN, newPIN string) (PINCheck, error) {
	if !validPIN(newPIN) {
		return PINCheck{}, ErrPINFormat
	}
	proof, left, err := s.reservePINAttempt(ctx, uid)
	if err != nil {
		return PINCheck{}, err
	}
	if !validPIN(oldPIN) || !verifyPIN(oldPIN, proof.hash) {
		return PINCheck{Left: left}, nil
	}
	if oldPIN == newPIN {
		if _, err := s.finishPINCheck(ctx, uid, proof, ""); err != nil {
			return PINCheck{}, err
		}
		return PINCheck{}, ErrPINSame
	}
	hash, err := HashPIN(newPIN)
	if err != nil {
		return PINCheck{}, err
	}
	return s.finishPINCheck(ctx, uid, proof, hash)
}
