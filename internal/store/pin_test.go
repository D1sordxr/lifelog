package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func init() { PINIterations = 1000 } // быстрые тесты; в работе — 600 000

func TestPIN(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := lt("2026-11-18 10:00")
	s.SetClock(func() time.Time { return now })
	u, _ := s.TelegramUser(ctx, 111, "me", true)

	if _, err := s.CheckPIN(ctx, u.ID, "123456"); err != ErrPINNotSet {
		t.Fatalf("check before set: %v", err)
	}
	for _, bad := range []string{"", "12345", "1234567", "12345a", "１２３４５６"} {
		if _, err := s.SetPIN(ctx, u.ID, bad); err != ErrPINFormat {
			t.Errorf("SetPIN(%q): %v", bad, err)
		}
	}
	if _, err := s.SetPIN(ctx, u.ID, "246810"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPIN(ctx, u.ID, "000000"); err != ErrPINSet {
		t.Fatalf("overwrite PIN: %v", err)
	}
	var stored string
	s.db.QueryRow(`SELECT pin_hash FROM users WHERE id = ?`, u.ID).Scan(&stored)
	if strings.Contains(stored, "246810") || !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Fatalf("pin stored badly: %s", stored)
	}

	if result, err := s.CheckPIN(ctx, u.ID, "246810"); !result.OK || err != nil {
		t.Fatalf("correct pin: %v %v", result.OK, err)
	}
	for i := 1; i <= 4; i++ {
		result, err := s.CheckPIN(ctx, u.ID, "111111")
		if result.OK || err != nil || result.Left != PINFreeAttempts-i {
			t.Fatalf("wrong #%d: ok=%v left=%d err=%v", i, result.OK, result.Left, err)
		}
	}
	// 5-я ошибка → блокировка на минуту; во время неё не проходит даже верный PIN.
	if result, _ := s.CheckPIN(ctx, u.ID, "111111"); result.OK || result.Left != 0 {
		t.Fatal("5th wrong")
	}
	var le *PINLockedError
	if _, err := s.CheckPIN(ctx, u.ID, "246810"); !errors.As(err, &le) || !le.Until.Equal(now.Add(time.Minute)) {
		t.Fatalf("locked: %v", err)
	}
	// после паузы: ещё ошибка → блокировка удваивается
	now = now.Add(61 * time.Second)
	s.CheckPIN(ctx, u.ID, "111111")
	if _, err := s.CheckPIN(ctx, u.ID, "246810"); !errors.As(err, &le) || !le.Until.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("escalation: %v", err)
	}
	// блокировка не больше часа
	for i := 0; i < 20; i++ {
		now = now.Add(2 * time.Hour)
		s.CheckPIN(ctx, u.ID, "111111")
	}
	if _, err := s.CheckPIN(ctx, u.ID, "246810"); !errors.As(err, &le) || le.Until.Sub(now) > time.Hour {
		t.Fatalf("cap: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if result, err := s.CheckPIN(ctx, u.ID, "246810"); !result.OK || err != nil {
		t.Fatalf("after lock: %v %v", result.OK, err)
	}
	if st, _ := s.PINStatus(ctx, u.ID); st.Failed != 0 || !st.LockedUntil.IsZero() {
		t.Fatalf("counter not reset: %+v", st)
	}

	if err := s.ResetPIN(ctx, 111); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.PINStatus(ctx, u.ID); st.Set {
		t.Fatal("reset")
	}
	if err := s.ResetPIN(ctx, 999); err != ErrUserNotFound {
		t.Fatalf("reset unknown: %v", err)
	}
}

func TestChangePIN(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := lt("2026-11-18 10:00")
	s.SetClock(func() time.Time { return now })
	u, _ := s.TelegramUser(ctx, 111, "me", true)
	s.SetPIN(ctx, u.ID, "246810")

	// Формат нового и совпадение со старым — ошибки, формат не тратит попытку.
	if _, err := s.ChangePIN(ctx, u.ID, "246810", "12ab56"); err != ErrPINFormat {
		t.Fatalf("format: %v", err)
	}
	if _, err := s.ChangePIN(ctx, u.ID, "246810", "246810"); err != ErrPINSame {
		t.Fatalf("same: %v", err)
	}
	// Неверный текущий — неудачная попытка; 5 подряд → блокировка, и смена не проходит даже с верным.
	for i := 1; i <= PINFreeAttempts; i++ {
		if result, err := s.ChangePIN(ctx, u.ID, "000000", "111111"); result.OK || err != nil {
			t.Fatalf("wrong old #%d: %v %v", i, result.OK, err)
		}
	}
	var le *PINLockedError
	if _, err := s.ChangePIN(ctx, u.ID, "246810", "111111"); !errors.As(err, &le) {
		t.Fatalf("change during lock: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if result, err := s.ChangePIN(ctx, u.ID, "246810", "111111"); !result.OK || err != nil {
		t.Fatalf("change: %v %v", result.OK, err)
	}
	if result, _ := s.CheckPIN(ctx, u.ID, "246810"); result.OK {
		t.Fatal("old PIN still works")
	}
	if result, _ := s.CheckPIN(ctx, u.ID, "111111"); !result.OK {
		t.Fatal("new PIN does not work")
	}
}

func TestPINProofBecomesStaleAfterChange(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.SetPIN(ctx, OwnerID, "246810"); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.reservePINAttempt(ctx, OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.reservePINAttempt(ctx, OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	nextHash, err := HashPIN("123456")
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := HashPIN("654321")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { _, err := s.finishPINCheck(ctx, OwnerID, first, nextHash); results <- err }()
	go func() { _, err := s.finishPINCheck(ctx, OwnerID, second, otherHash); results <- err }()
	ok, stale := 0, 0
	for range 2 {
		switch err := <-results; err {
		case nil:
			ok++
		case ErrPINChanged:
			stale++
		default:
			t.Error(err)
		}
	}
	if ok != 1 || stale != 1 {
		t.Fatalf("смен PIN %d, устаревших проверок %d", ok, stale)
	}
	if _, err := s.CheckPIN(ctx, OwnerID, "000000"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishPINCheck(ctx, OwnerID, first, ""); err != ErrPINChanged {
		t.Fatalf("старый PIN всё ещё может сбросить счётчик: %v", err)
	}
	state, err := s.PINStatus(ctx, OwnerID)
	if err != nil || state.Failed != 1 {
		t.Fatalf("счётчик изменился: %+v %v", state, err)
	}
}
