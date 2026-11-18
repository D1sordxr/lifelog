//go:build !windows

package main

func defaultDBPath() string { return "lifelog.db" }

// defaultEnvFile: на Linux env-файл подключает systemd (EnvironmentFile=) или ENV_FILE.
func defaultEnvFile() string { return "" }

// Пути службы (deploy/linux) для ручных команд backup и reset-pin.
func adminEnvFile() string { return "/etc/lifelog/lifelog.env" }
func adminDBPath() string  { return "/var/lib/lifelog/lifelog.db" }

func platformCommand(string) (bool, error) { return false, nil }

func runPlatform(cfg config) error { return runInteractive(cfg) }
