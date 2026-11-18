package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lifelog/internal/store"
)

// PIN — второй барьер после входа через Telegram, как экран блокировки iPhone.
//
//   - cookie разблокировки привязана к сессии и версии PIN;
//   - разблокировка живёт unlockIdle с последнего запроса, потом снова нужен PIN;
//   - PIN проверяется только на сервере; счётчик попыток и блокировка хранятся в
//     базе у пользователя — новая сессия или очистка cookies их не сбрасывают;
//   - задать PIN можно, только если он не задан; сменить — только зная текущий
//     (неверный текущий засчитывается как неудачная попытка); сбросить — только
//     командой `lifelog reset-pin <telegram id>` на самом сервере.

const (
	unlockCookie = "lifelog_unlock"
	unlockIdle   = 10 * time.Minute
	pinFormLimit = 1 << 10
)

type pinData struct {
	Mode   string // setup | unlock | change
	Length int
}

// pinGate пропускает запрос, если PIN задан и сессия разблокирована;
// иначе отдаёт экран PIN (или 401 для API).
func (s *Server) pinGate(w http.ResponseWriter, r *http.Request, u *store.User) (*http.Request, bool) {
	if r.Method == http.MethodPost && r.URL.Path == "/pin" {
		return r, true // обработчик сам проверяет PIN
	}
	st, err := s.st.PINStatus(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return r, false
	}
	now := s.opt.Now()
	if st.Set {
		if c, err := r.Cookie(unlockCookie); err == nil {
			sess := sessionFrom(r)
			if uid, err := s.verifyToken("unlock", c.Value, now, unlockBinding(sess.token, st.Version)); err == nil && uid == u.ID {
				if r.Method == http.MethodPost && r.URL.Path == "/pin/lock" {
					sess.pinVersion = st.Version
					r = r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, sess))
				} else if r.Method != http.MethodPost || r.URL.Path != "/pin/change" {
					s.setUnlock(w, r, u.ID, st.Version) // скользящий срок
				}
				return r, true
			}
		}
	}
	if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/export.") {
		s.render(w, http.StatusOK, "pin.html", page{Title: "PIN", Bare: true, Script: "pin.js",
			Data: pinData{Mode: map[bool]string{true: "setup", false: "unlock"}[!st.Set], Length: store.PINLength}})
	} else {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "заблокировано — нужен PIN", "locked": true})
	}
	return r, false
}

func unlockBinding(token string, version int64) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:]) + "|" + strconv.FormatInt(version, 10)
}

func (s *Server) setUnlock(w http.ResponseWriter, r *http.Request, uid, version int64) {
	binding := unlockBinding(sessionFrom(r).token, version)
	http.SetCookie(w, &http.Cookie{
		Name: unlockCookie, Value: s.signToken("unlock", uid, s.opt.Now().Add(unlockIdle), binding),
		Path: "/", MaxAge: int(unlockIdle / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode, Partitioned: true,
	})
}

// pinSubmit — POST /pin: задать PIN (pin + pin2, только если не задан) или разблокировать.
func (s *Server) pinSubmit(w http.ResponseWriter, r *http.Request) {
	u, ok := r.Context().Value(userKey{}).(*store.User)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !readSmallForm(w, r, pinFormLimit) {
		return
	}
	ctx := r.Context()
	pin := r.PostForm.Get("pin")
	st, err := s.st.PINStatus(ctx, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !st.Set {
		if pin != r.PostForm.Get("pin2") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "PIN не совпадает"})
			return
		}
		version, err := s.st.SetPIN(ctx, u.ID, pin)
		switch {
		case errors.Is(err, store.ErrPINFormat):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		case errors.Is(err, store.ErrPINSet):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		case err != nil:
			s.fail(w, err)
			return
		}
		s.setUnlock(w, r, u.ID, version)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	result, err := s.st.CheckPIN(ctx, u.ID, pin)
	s.pinResult(w, r, u, result, err)
}

// pinChangePage — GET /pin/change (только из разблокированной сессии).
func (s *Server) pinChangePage(w http.ResponseWriter, r *http.Request) {
	if !s.tgMode() {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, "pin.html", page{Title: "Смена PIN", Bare: true, Script: "pin.js",
		Data: pinData{Mode: "change", Length: store.PINLength}})
}

// pinChange — POST /pin/change: old, pin, pin2.
func (s *Server) pinChange(w http.ResponseWriter, r *http.Request) {
	u, ok := r.Context().Value(userKey{}).(*store.User)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !readSmallForm(w, r, pinFormLimit) {
		return
	}
	pin := r.PostForm.Get("pin")
	if pin != r.PostForm.Get("pin2") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Новый PIN не совпадает", "step": "new"})
		return
	}
	result, err := s.st.ChangePIN(r.Context(), u.ID, r.PostForm.Get("old"), pin)
	switch {
	case errors.Is(err, store.ErrPINFormat), errors.Is(err, store.ErrPINSame):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "step": "new"})
		return
	}
	s.pinResult(w, r, u, result, err)
}

// pinResult — общий ответ на проверку PIN: блокировка, неверный, успех.
func (s *Server) pinResult(w http.ResponseWriter, r *http.Request, u *store.User, result store.PINCheck, err error) {
	ctx := r.Context()
	var locked *store.PINLockedError
	tooMany := func(prefix string, until time.Time) {
		wait := until.Sub(s.opt.Now())
		sec := int(math.Ceil(wait.Seconds()))
		w.Header().Set("Retry-After", fmt.Sprint(sec))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": prefix + "Повтори через " + humanWait(wait), "retry_after": sec,
		})
	}
	switch {
	case errors.As(err, &locked):
		tooMany("Слишком много попыток. ", locked.Until)
	case errors.Is(err, store.ErrPINChanged):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "locked": true})
	case err != nil:
		s.fail(w, err)
	case !result.OK && result.Left == 0:
		// эта ошибка включила блокировку — сразу сообщаем
		if st, err := s.st.PINStatus(ctx, u.ID); err == nil && st.LockedUntil.After(s.opt.Now()) {
			tooMany("Неверный PIN. ", st.LockedUntil)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Неверный PIN", "left": 0})
	case !result.OK:
		msg := "Неверный PIN"
		if result.Left > 0 {
			msg += fmt.Sprintf(" (осталось попыток: %d)", result.Left)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg, "left": result.Left})
	default:
		s.setUnlock(w, r, u.ID, result.Version)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// pinLock — POST /pin/lock: отозвать разблокировки во всех сессиях.
func (s *Server) pinLock(w http.ResponseWriter, r *http.Request) {
	u, ok := r.Context().Value(userKey{}).(*store.User)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !readSmallForm(w, r, pinFormLimit) {
		return
	}
	if err := s.st.LockPIN(r.Context(), u.ID, sessionFrom(r).pinVersion); err != nil {
		s.fail(w, err)
		return
	}
	clearUnlock(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func clearUnlock(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: unlockCookie, Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteNoneMode, Partitioned: true})
}

func humanWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d с", int(math.Ceil(d.Seconds())))
	}
	return fmt.Sprintf("%d мин", int(math.Ceil(d.Minutes())))
}
