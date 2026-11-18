package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// BackupTo делает консистентную копию базы через VACUUM INTO.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// DailyBackup создаёт dir/lifelog-YYYY-MM-DD.db, если за сегодня (локально)
// копии ещё нет, и оставляет только keep последних копий.
// Возвращает путь к новой копии или "" если копия уже была.
func (s *Store) DailyBackup(ctx context.Context, dir string, keep int) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	target := filepath.Join(dir, "lifelog-"+s.now().In(s.loc).Format("2006-01-02")+".db")
	created := ""
	if _, err := os.Stat(target); os.IsNotExist(err) {
		created, err = s.createDailyBackup(ctx, target)
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return created, pruneBackups(dir, keep)
}

func (s *Store) createDailyBackup(ctx context.Context, target string) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(target), ".lifelog-backup-*")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := s.BackupTo(ctx, tmp); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	// Публикуем готовую копию атомарно, сохраняя уже опубликованную другим процессом.
	if err := os.Link(tmp, target); err != nil {
		if os.IsExist(err) {
			return "", nil
		}
		return "", err
	}
	return target, nil
}

func pruneBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "lifelog-????-??-??.db"))
	if err != nil {
		return err
	}
	sort.Strings(files) // дата в имени → лексикографический порядок = хронологический
	for len(files) > keep {
		if err := os.Remove(files[0]); err != nil && !os.IsNotExist(err) {
			return err
		}
		files = files[1:]
	}
	return nil
}

// BackupLoop делает DailyBackup сразу и затем каждый час, пока ctx жив.
func (s *Store) BackupLoop(ctx context.Context, dir string, keep int, logf func(string, ...any)) {
	run := func() {
		path, err := s.DailyBackup(ctx, dir, keep)
		switch {
		case err != nil:
			logf("backup error: %v", err)
		case path != "":
			logf("backup created: %s", path)
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
