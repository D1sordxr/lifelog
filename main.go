// lifelog — личный веб-журнал дня: подъём, тренировка, энергия, отчёт, отбой.
//
// Запуск:   lifelog            — HTTP-сервер (адрес из ADDR)
//
//	lifelog backup     — разовая копия базы в BACKUP_DIR
//	lifelog install    — (Windows) установить/обновить службу
//	lifelog uninstall  — (Windows) удалить службу (данные остаются)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // зоны часовых поясов внутри бинарника (на Windows их нет в системе)

	"lifelog/internal/store"
	"lifelog/internal/tg"
	"lifelog/internal/web"
)

// envKeys — все переменные окружения, которые читает приложение.
var envKeys = []string{"ADDR", "DB_PATH", "TZ", "BACKUP_DIR", "BACKUP_KEEP", "BASIC_AUTH_USER", "BASIC_AUTH_PASS",
	"BOT_TOKEN", "ALLOWED_USERS", "WEBAPP_URL"}

type config struct {
	Addr       string
	DBPath     string
	Loc        *time.Location
	BackupDir  string
	BackupKeep int
	AuthUser   string
	AuthPass   string
	BotToken   string       // включает вход через Telegram Mini App
	Allowed    tg.Allowlist // ALLOWED_USERS: id и/или @username через запятую
	WebAppURL  string       // WEBAPP_URL: адрес приложения для кнопок бота
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		Addr:      env("ADDR", "127.0.0.1:8080"),
		DBPath:    env("DB_PATH", defaultDBPath()),
		Loc:       time.Local,
		AuthUser:  os.Getenv("BASIC_AUTH_USER"),
		AuthPass:  os.Getenv("BASIC_AUTH_PASS"),
		BotToken:  strings.TrimSpace(os.Getenv("BOT_TOKEN")),
		Allowed:   tg.ParseAllowlist(tg.SplitList(os.Getenv("ALLOWED_USERS"))),
		WebAppURL: strings.TrimSpace(os.Getenv("WEBAPP_URL")),
	}
	if c.BotToken != "" && c.Allowed.Len() == 0 {
		return c, fmt.Errorf("BOT_TOKEN задан, а ALLOWED_USERS пуст — войти не сможет никто")
	}
	if c.WebAppURL != "" && !strings.HasPrefix(c.WebAppURL, "https://") {
		return c, fmt.Errorf("WEBAPP_URL должен начинаться с https:// (требование Telegram)")
	}
	if tz := os.Getenv("TZ"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return c, fmt.Errorf("TZ=%q: %w", tz, err)
		}
		c.Loc = loc
	}
	c.BackupDir = env("BACKUP_DIR", filepath.Join(filepath.Dir(c.DBPath), "backups"))
	keep, err := strconv.Atoi(env("BACKUP_KEEP", "30"))
	if err != nil {
		return c, fmt.Errorf("BACKUP_KEEP: %w", err)
	}
	c.BackupKeep = keep
	return c, nil
}

func main() {
	log.SetFlags(log.LstdFlags)
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if handled, err := platformCommand(cmd); handled {
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "":
		err = runPlatform(cfg)
	case "backup":
		err = backupOnce(cfg)
	case "-h", "--help", "help":
		fmt.Println("usage: lifelog [backup|install|uninstall]")
		return
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// runInteractive — обычный запуск из консоли/systemd: до Ctrl+C или SIGTERM.
func runInteractive(cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, cfg)
}

// run поднимает базу, бэкапы и HTTP-сервер до отмены ctx.
func run(ctx context.Context, cfg config) error {
	if dir := filepath.Dir(cfg.DBPath); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	st, err := store.Open(cfg.DBPath, cfg.Loc)
	if err != nil {
		return err
	}
	defer st.Close()

	if host, _, err := net.SplitHostPort(cfg.Addr); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Printf("WARNING: ADDR=%s слушает не только localhost", cfg.Addr)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go st.BackupLoop(ctx, cfg.BackupDir, cfg.BackupKeep, log.Printf)
	if cfg.BotToken != "" && cfg.WebAppURL != "" {
		bot := &tg.Bot{Token: cfg.BotToken, WebAppURL: cfg.WebAppURL, Allow: cfg.Allowed, Logf: log.Printf}
		go bot.Run(ctx)
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: web.New(st, web.Options{
			AuthUser: cfg.AuthUser, AuthPass: cfg.AuthPass,
			BotToken: cfg.BotToken, Allow: cfg.Allowed,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	auth := "без входа"
	if cfg.BotToken != "" {
		auth = fmt.Sprintf("Telegram, разрешено записей: %d", cfg.Allowed.Len())
		if cfg.WebAppURL == "" {
			auth += ", бот выключен (нет WEBAPP_URL)"
		}
	}
	log.Printf("lifelog: http://%s  db=%s  tz=%s  вход: %s", cfg.Addr, cfg.DBPath, cfg.Loc, auth)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func backupOnce(cfg config) error {
	st, err := store.Open(cfg.DBPath, cfg.Loc)
	if err != nil {
		return err
	}
	defer st.Close()
	path, err := st.DailyBackup(context.Background(), cfg.BackupDir, cfg.BackupKeep)
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Println("сегодняшняя копия уже есть в", cfg.BackupDir)
	} else {
		fmt.Println("создана копия", path)
	}
	return nil
}
