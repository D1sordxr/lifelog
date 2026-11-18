// Package ru — форматирование дат и длительностей по-русски.
package ru

import (
	"fmt"
	"time"
)

var weekdays = [...]string{"воскресенье", "понедельник", "вторник", "среда", "четверг", "пятница", "суббота"}
var weekdaysShort = [...]string{"Вс", "Пн", "Вт", "Ср", "Чт", "Пт", "Сб"}
var monthsGen = [...]string{"", "января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
var monthsShort = [...]string{"", "янв", "фев", "мар", "апр", "мая", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}

// DateLong — «вторник, 18 ноября».
func DateLong(t time.Time) string {
	return fmt.Sprintf("%s, %d %s", weekdays[t.Weekday()], t.Day(), monthsGen[t.Month()])
}

// DateShort — «Вт, 18 ноя» (+ год, если не текущий относительно now).
func DateShort(t, now time.Time) string {
	s := fmt.Sprintf("%s, %d %s", weekdaysShort[t.Weekday()], t.Day(), monthsShort[t.Month()])
	if t.Year() != now.Year() {
		s += fmt.Sprintf(" %d", t.Year())
	}
	return s
}

// Weekday — «Вт».
func Weekday(t time.Time) string { return weekdaysShort[t.Weekday()] }

// Dur — длительность как «7:05». nil → «—».
func Dur(d *time.Duration) string {
	if d == nil {
		return "—"
	}
	m := int(d.Round(time.Minute) / time.Minute)
	return fmt.Sprintf("%d:%02d", m/60, m%60)
}
