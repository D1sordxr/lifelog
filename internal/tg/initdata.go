// Package tg — Telegram: проверка initData мини-приложения, allowlist и бот.
package tg

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WebAppUser — пользователь из подписанного initData.
type WebAppUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

var (
	ErrBadSignature = errors.New("initData: неверная подпись")
	ErrExpired      = errors.New("initData: устарели (auth_date)")
	ErrMalformed    = errors.New("initData: некорректный формат")
)

// ValidateInitData проверяет строку Telegram.WebApp.initData целиком:
// HMAC-SHA256 подпись (ключ — HMAC("WebAppData", токен бота)) и свежесть auth_date.
// Данным из initDataUnsafe на клиенте доверять нельзя — только этой проверке.
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app
func ValidateInitData(initData, botToken string, maxAge time.Duration, now time.Time) (*WebAppUser, error) {
	if botToken == "" {
		return nil, errors.New("initData: не задан токен бота")
	}
	vals, err := url.ParseQuery(initData)
	if err != nil {
		return nil, ErrMalformed
	}
	hash := vals.Get("hash")
	if hash == "" {
		return nil, ErrMalformed
	}
	got, err := hex.DecodeString(hash)
	if err != nil {
		return nil, ErrBadSignature
	}

	keys := make([]string, 0, len(vals))
	for k := range vals {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+vals.Get(k))
	}
	if !hmac.Equal(got, sign(botToken, strings.Join(lines, "\n"))) {
		return nil, ErrBadSignature
	}

	authDate, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil {
		return nil, ErrMalformed
	}
	age := now.Sub(time.Unix(authDate, 0))
	if age > maxAge || age < -5*time.Minute {
		return nil, ErrExpired
	}

	var u WebAppUser
	if err := json.Unmarshal([]byte(vals.Get("user")), &u); err != nil || u.ID == 0 {
		return nil, ErrMalformed
	}
	return &u, nil
}

func sign(botToken, dataCheckString string) []byte {
	k := hmac.New(sha256.New, []byte("WebAppData"))
	k.Write([]byte(botToken))
	m := hmac.New(sha256.New, k.Sum(nil))
	m.Write([]byte(dataCheckString))
	return m.Sum(nil)
}

// SignInitData собирает подписанную строку initData — для тестов.
func SignInitData(botToken string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	vals := url.Values{}
	for _, k := range keys {
		lines = append(lines, k+"="+fields[k])
		vals.Set(k, fields[k])
	}
	vals.Set("hash", hex.EncodeToString(sign(botToken, strings.Join(lines, "\n"))))
	return vals.Encode()
}
