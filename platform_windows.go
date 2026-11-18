//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName    = "lifelog"
	serviceAccount = `NT SERVICE\lifelog` // виртуальная учётная запись службы, без пароля
)

func programData() string  { return env("ProgramData", `C:\ProgramData`) }
func programFiles() string { return env("ProgramFiles", `C:\Program Files`) }

func defaultDataDir() string { return filepath.Join(programData(), "lifelog") }
func installedExe() string   { return filepath.Join(programFiles(), "lifelog", "lifelog.exe") }

// defaultEnvFile — env-файл службы. В обычном запуске (не служба) не используется.
func defaultEnvFile() string {
	if isSvc, _ := svc.IsWindowsService(); isSvc {
		return filepath.Join(defaultDataDir(), "lifelog.env")
	}
	return ""
}

// Пути службы для ручных команд backup и reset-pin.
func adminEnvFile() string { return filepath.Join(defaultDataDir(), "lifelog.env") }
func adminDBPath() string  { return filepath.Join(defaultDataDir(), "lifelog.db") }

func defaultDBPath() string {
	if isSvc, _ := svc.IsWindowsService(); isSvc {
		return filepath.Join(defaultDataDir(), "lifelog.db")
	}
	return "lifelog.db"
}

func platformCommand(cmd string) (bool, error) {
	switch cmd {
	case "install":
		return true, install()
	case "uninstall":
		return true, uninstall()
	}
	return false, nil
}

// runPlatform: под SCM работаем как служба, иначе — как обычная программа.
func runPlatform(cfg config) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		return runInteractive(cfg)
	}
	if f := openServiceLog(filepath.Join(filepath.Dir(cfg.DBPath), "lifelog.log")); f != nil {
		log.SetOutput(f)
		defer f.Close()
	}
	return svc.Run(serviceName, &service{cfg: cfg})
}

func openServiceLog(path string) io.WriteCloser {
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		os.Rename(path, path+".old")
	}
	os.MkdirAll(filepath.Dir(path), 0o750)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil
	}
	return f
}

type service struct{ cfg config }

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, s.cfg) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err != nil {
				log.Printf("stopped with error: %v", err)
				return false, 1 // ненулевой код → SCM перезапустит службу
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// install копирует exe в Program Files, создаёт (или обновляет) службу
// с автозапуском и перезапуском при сбоях, выдаёт права на папку данных и
// запускает службу. Запускать из терминала администратора.
//
// Переменные из envKeys, заданные в текущем окружении, сохраняются в env-файл
// службы поверх прежних (остальные не меняются; "-" — закомментировать).
func install() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нет доступа к диспетчеру служб — запусти терминал от имени администратора: %w", err)
	}
	defer m.Disconnect()

	// 1. Остановить работающую службу, чтобы можно было заменить exe.
	if s, err := m.OpenService(serviceName); err == nil {
		stopService(s)
		s.Close()
	}

	// 2. Скопировать exe.
	src, err := os.Executable()
	if err != nil {
		return err
	}
	dst := installedExe()
	if !strings.EqualFold(filepath.Clean(src), filepath.Clean(dst)) {
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("копирование в %s: %w", dst, err)
		}
		fmt.Println("exe:", dst)
	}

	// 3. Создать или обновить службу.
	s, err := m.OpenService(serviceName)
	if err != nil {
		s, err = m.CreateService(serviceName, dst, mgr.Config{
			DisplayName:      "Lifelog",
			Description:      "Личный веб-журнал дня (127.0.0.1, наружу через tailscale serve).",
			StartType:        mgr.StartAutomatic,
			ServiceStartName: serviceAccount,
		})
		if err != nil {
			return fmt.Errorf("создание службы: %w", err)
		}
		fmt.Println("служба создана:", serviceName)
	} else {
		c, err := s.Config()
		if err != nil {
			return err
		}
		c.BinaryPathName = `"` + dst + `"`
		c.StartType = mgr.StartAutomatic
		if err := s.UpdateConfig(c); err != nil {
			return fmt.Errorf("обновление службы: %w", err)
		}
		fmt.Println("служба обновлена:", serviceName)
	}
	defer s.Close()

	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 24*60*60); err != nil {
		return fmt.Errorf("recovery: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("recovery: %w", err)
	}

	// 4. Папка данных: только администраторы, SYSTEM и сама служба
	//    (там же env-файл с токеном бота).
	dataDir := defaultDataDir()
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	dirs := []string{dataDir}
	if p := os.Getenv("DB_PATH"); p != "" && !strings.EqualFold(filepath.Dir(p), dataDir) {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		dirs = append(dirs, filepath.Dir(p))
	}
	for _, dir := range dirs {
		// SID вместо имён групп — имена локализованы («Администраторы»).
		out, err := exec.Command("icacls", dir, "/inheritance:r",
			"/grant:r", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F", serviceAccount+":(OI)(CI)M").CombinedOutput()
		if err != nil {
			return fmt.Errorf("icacls %s: %v: %s", dir, err, out)
		}
	}
	fmt.Println("данные:", dataDir)

	// 5. Настройки — в env-файл (переменные из реестра прежних версий переносятся туда).
	if err := saveServiceEnv(filepath.Join(dataDir, "lifelog.env")); err != nil {
		return err
	}

	// 6. Запуск.
	if err := s.Start(); err != nil {
		return fmt.Errorf("запуск службы: %w (лог: %s)", err, filepath.Join(dataDir, "lifelog.log"))
	}
	fmt.Println("служба запущена. Проверка: curl http://127.0.0.1:8080/healthz")
	fmt.Println("доступ из tailnet (один раз): tailscale serve --bg 8080")
	return nil
}

func uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("нужен терминал администратора: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("служба %s не найдена", serviceName)
	}
	defer s.Close()
	stopService(s)
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("служба удалена. Данные остались в", defaultDataDir(), "— exe в", filepath.Dir(installedExe()))
	return nil
}

func stopService(s *mgr.Service) {
	st, err := s.Query()
	if err != nil || st.State == svc.Stopped {
		return
	}
	s.Control(svc.Stop)
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if st, err := s.Query(); err != nil || st.State == svc.Stopped {
			return
		}
	}
}

const serviceKey = `SYSTEM\CurrentControlSet\Services\` + serviceName

// saveServiceEnv пишет заданные переменные в значение Environment службы.
// saveServiceEnv записывает в env-файл переменные, заданные сейчас в окружении,
// поверх прежних ("-" — закомментировать), переносит туда значения из реестра
// (так хранили настройки прежние версии) и удаляет их из реестра.
func saveServiceEnv(path string) error {
	data, err := os.ReadFile(path)
	created := false
	if os.IsNotExist(err) {
		data, created = []byte(envTemplate), true
	} else if err != nil {
		return err
	}
	current := parseEnvFile(data)
	set := map[string]string{}
	legacy := serviceEnv()
	for k, v := range legacy {
		if current[k] == "" {
			set[k] = v
		}
	}
	for _, k := range envKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			set[k] = v
		}
	}
	if len(set) > 0 || created {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, updateEnvFile(data, set, envKeys), 0o640); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	if len(legacy) > 0 {
		if key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey, registry.SET_VALUE); err == nil {
			key.DeleteValue("Environment")
			key.Close()
		}
	}
	for _, k := range envKeys {
		v, ok := set[k]
		if !ok {
			continue
		}
		if (k == "BOT_TOKEN" || k == "BASIC_AUTH_PASS") && v != "-" {
			v = "***"
		}
		fmt.Printf("env: %s=%s\n", k, v)
	}
	fmt.Println("настройки:", path)
	return nil
}

func serviceEnv() map[string]string {
	out := map[string]string{}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey, registry.QUERY_VALUE)
	if err != nil {
		return out
	}
	defer key.Close()
	vals, _, err := key.GetStringsValue("Environment")
	if err != nil {
		return out
	}
	for _, kv := range vals {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Служба остановлена, но Windows может ещё недолго держать файл.
	for i := 0; ; i++ {
		err = os.Rename(tmp, dst)
		if err == nil || i == 20 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
