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
// Переменные из envKeys, заданные в текущем окружении, сохраняются в
// настройки службы поверх прежних (остальные не меняются; "-" — удалить).
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

	// 4. Окружение службы.
	if err := saveServiceEnv(); err != nil {
		return err
	}

	// 5. Папка данных: только администраторы, SYSTEM и сама служба.
	dataDir := defaultDataDir()
	if p := os.Getenv("DB_PATH"); p != "" {
		dataDir = filepath.Dir(p)
	} else if p := serviceEnv()["DB_PATH"]; p != "" {
		dataDir = filepath.Dir(p)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	// SID вместо имён групп — имена локализованы («Администраторы»).
	out, err := exec.Command("icacls", dataDir, "/inheritance:r",
		"/grant:r", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F", serviceAccount+":(OI)(CI)M").CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls %s: %v: %s", dataDir, err, out)
	}
	fmt.Println("данные:", dataDir)

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
func saveServiceEnv() error {
	// Слияние с прежними настройками: заданные сейчас переменные заменяют старые,
	// остальные сохраняются; значение "-" удаляет переменную.
	vars := serviceEnv()
	changed := false
	for _, k := range envKeys {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			continue
		}
		changed = true
		if v == "-" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	if !changed {
		return nil
	}
	var list []string
	for _, k := range envKeys {
		if v, ok := vars[k]; ok {
			list = append(list, k+"="+v)
		}
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("реестр: %w", err)
	}
	defer key.Close()
	if len(list) == 0 {
		err = key.DeleteValue("Environment")
	} else {
		err = key.SetStringsValue("Environment", list)
	}
	if err != nil {
		return fmt.Errorf("реестр: %w", err)
	}
	for _, v := range list {
		for _, secret := range []string{"BASIC_AUTH_PASS=", "BOT_TOKEN="} {
			if strings.HasPrefix(v, secret) {
				v = secret + "***"
			}
		}
		fmt.Println("env:", v)
	}
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
