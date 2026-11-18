package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Field — редактируемое поле дня. Через этот реестр идут автосохранение,
// сохранение формы целиком и импорт CSV, поэтому правила разбора и проверки
// живут в одном месте.
//
// Как добавить поле: миграция → поле в Day → dayCols/scanDay/insertDay/updateDay
// → запись здесь → input в шаблоне → (по желанию) колонка в экспорте.
type Field struct {
	Name string
	// Set разбирает строку из формы/CSV и записывает в d.
	// Значение "now" у полей-времени означает текущий момент.
	Set func(d *Day, v string, loc *time.Location, now time.Time) error
	// Get возвращает значение в виде, пригодном для value="" в форме.
	Get func(d *Day, loc *time.Location) string
}

// FormTimeLayout — формат <input type="datetime-local">.
const FormTimeLayout = "2006-01-02T15:04"

var Fields = []Field{
	{
		Name: "wake_at",
		Set: func(d *Day, v string, loc *time.Location, now time.Time) error {
			t, err := parseTime(v, loc, now)
			if err != nil {
				return err
			}
			if t == nil {
				return errors.New("время подъёма обязательно")
			}
			d.WakeAt = *t
			return nil
		},
		Get: func(d *Day, loc *time.Location) string { return formTime(&d.WakeAt, loc) },
	},
	{
		Name: "bed_at",
		Set: func(d *Day, v string, loc *time.Location, now time.Time) (err error) {
			d.BedAt, err = parseTime(v, loc, now)
			return err
		},
		Get: func(d *Day, loc *time.Location) string { return formTime(d.BedAt, loc) },
	},
	{
		Name: "train_start",
		Set: func(d *Day, v string, loc *time.Location, now time.Time) (err error) {
			d.TrainStart, err = parseTime(v, loc, now)
			return err
		},
		Get: func(d *Day, loc *time.Location) string { return formTime(d.TrainStart, loc) },
	},
	{
		Name: "train_minutes",
		Set: func(d *Day, v string, _ *time.Location, _ time.Time) (err error) {
			d.TrainMinutes, err = parseInt(v, 0, 24*60, "длительность — число минут от 0 до 1440")
			return err
		},
		Get: func(d *Day, _ *time.Location) string { return intStr(d.TrainMinutes) },
	},
	{
		Name: "train_type",
		Set: func(d *Day, v string, _ *time.Location, _ time.Time) error {
			v = strings.TrimSpace(v)
			if utf8.RuneCountInString(v) > 100 {
				return errors.New("тип тренировки — не длиннее 100 символов")
			}
			d.TrainType = v
			return nil
		},
		Get: func(d *Day, _ *time.Location) string { return d.TrainType },
	},
	{
		Name: "energy",
		Set: func(d *Day, v string, _ *time.Location, _ time.Time) (err error) {
			d.Energy, err = parseInt(v, 1, 5, "энергия — от 1 до 5")
			return err
		},
		Get: func(d *Day, _ *time.Location) string { return intStr(d.Energy) },
	},
	{
		Name: "report",
		Set: func(d *Day, v string, _ *time.Location, _ time.Time) error {
			d.Report = strings.TrimRight(strings.ReplaceAll(v, "\r\n", "\n"), " \t\n")
			return nil
		},
		Get: func(d *Day, _ *time.Location) string { return d.Report },
	},
}

// InputError — ошибка во введённых данных (в отличие от ошибок базы).
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// FieldByName ищет поле в реестре.
func FieldByName(name string) (Field, bool) {
	for _, f := range Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Validate — проверки, затрагивающие несколько полей.
func (d *Day) Validate() error {
	if d.WakeAt.IsZero() {
		return errors.New("время подъёма обязательно")
	}
	if d.BedAt != nil && !d.BedAt.After(d.WakeAt) {
		return errors.New("отбой должен быть позже подъёма")
	}
	if d.BedAt != nil && d.BedAt.Sub(d.WakeAt) > 48*time.Hour {
		return errors.New("между подъёмом и отбоем больше 48 часов — проверь даты")
	}
	return nil
}

// SetField меняет одно поле дня и сохраняет. Возвращает обновлённый день.
func (d *Days) SetField(ctx context.Context, id int64, name, value string) (*Day, error) {
	f, ok := FieldByName(name)
	if !ok {
		return nil, &InputError{fmt.Errorf("неизвестное поле %q", name)}
	}
	day, err := d.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := f.Set(day, value, d.s.loc, d.s.now()); err != nil {
		return nil, &InputError{err}
	}
	if err := d.Save(ctx, day); err != nil {
		return nil, err
	}
	return day, nil
}

// SetFields меняет несколько полей за раз (сохранение формы без JS).
// Поля, которых нет в values, не трогаются.
func (d *Days) SetFields(ctx context.Context, id int64, values map[string]string) (*Day, error) {
	day, err := d.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := d.s.now()
	for _, f := range Fields {
		v, ok := values[f.Name]
		if !ok {
			continue
		}
		if err := f.Set(day, v, d.s.loc, now); err != nil {
			return nil, &InputError{fmt.Errorf("%s: %w", f.Name, err)}
		}
	}
	if err := d.Save(ctx, day); err != nil {
		return nil, err
	}
	return day, nil
}

func parseTime(v string, loc *time.Location, now time.Time) (*time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if v == "now" {
		t := now.UTC().Truncate(time.Second)
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		t = t.UTC()
		return &t, nil
	}
	for _, layout := range []string{FormTimeLayout, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("не понимаю время %q", v)
}

func formTime(t *time.Time, loc *time.Location) string {
	if t == nil {
		return ""
	}
	return t.In(loc).Format(FormTimeLayout)
}

func parseInt(v string, lo, hi int, msg string) (*int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return nil, errors.New(msg)
	}
	return &n, nil
}

func intStr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}
