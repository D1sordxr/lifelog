package tg

import (
	"strconv"
	"strings"
)

// Allowlist — кому разрешён вход. Записи — Telegram id (число) или username
// (с @ или без). Пользователь проходит, если совпал id ИЛИ username.
// Первая запись — владелец: при первом входе ему достаются записи,
// сделанные до включения Telegram.
type Allowlist struct {
	entries []entry
}

type entry struct {
	id       int64
	username string // в нижнем регистре, без @
}

// ParseAllowlist разбирает список вроде "123456789, @someone, other".
func ParseAllowlist(items []string) Allowlist {
	var a Allowlist
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if id, err := strconv.ParseInt(it, 10, 64); err == nil {
			a.entries = append(a.entries, entry{id: id})
			continue
		}
		a.entries = append(a.entries, entry{username: normUsername(it)})
	}
	return a
}

// SplitList делит строку переменной окружения по запятым и пробелам.
func SplitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' })
}

func normUsername(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

func (e entry) match(id int64, username string) bool {
	if e.id != 0 {
		return id != 0 && e.id == id
	}
	return username != "" && e.username == normUsername(username)
}

// Allowed — разрешён ли пользователь.
func (a Allowlist) Allowed(id int64, username string) bool {
	for _, e := range a.entries {
		if e.match(id, username) {
			return true
		}
	}
	return false
}

// IsOwner — совпадает ли пользователь с первой записью списка.
func (a Allowlist) IsOwner(id int64, username string) bool {
	return len(a.entries) > 0 && a.entries[0].match(id, username)
}

// Len — число записей.
func (a Allowlist) Len() int { return len(a.entries) }
