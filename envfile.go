package main

import (
	"bytes"
	"os"
	"strings"
)

// Env-файл службы: строки KEY=VALUE, # — комментарий. Пустое значение = не задано.
// Переменные окружения процесса важнее файла.

// envTemplate — содержимое нового env-файла.
const envTemplate = `# Настройки lifelog. Пустое значение — по умолчанию.
# После правки перезапустите службу (Windows: powershell Restart-Service lifelog).
TZ=
BOT_TOKEN=
ALLOWED_USERS=
WEBAPP_URL=
# ADDR=127.0.0.1:8080
# DB_PATH=
# BACKUP_DIR=
# BACKUP_KEEP=30
# BASIC_AUTH_USER=
# BASIC_AUTH_PASS=
`

var fileEnv = map[string]string{}

// getenv: окружение процесса, затем env-файл.
func getenv(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fileEnv[key]
}

// readEnvFile читает файл; отсутствующий файл — не ошибка.
func readEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseEnvFile(data), nil
}

func envLines(data []byte) []string {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func parseEnvFile(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range envLines(data) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// updateEnvFile записывает значения в текст файла, сохраняя остальные строки и
// комментарии: заменяет строку KEY=, раскомментирует "# KEY=" или дописывает в конец.
// Значение "-" закомментирует переменную.
func updateEnvFile(data []byte, set map[string]string, order []string) []byte {
	lines := envLines(data)
	if len(data) == 0 {
		lines = nil
	}
	keyOf := func(line string) (string, bool) {
		t := strings.TrimSpace(line)
		commented := strings.HasPrefix(t, "#")
		t = strings.TrimSpace(strings.TrimPrefix(t, "#"))
		k, _, ok := strings.Cut(t, "=")
		if !ok || strings.ContainsAny(k, " \t") {
			return "", commented
		}
		return k, commented
	}
	for _, k := range order {
		v, ok := set[k]
		if !ok {
			continue
		}
		newLine := k + "=" + v
		if v == "-" {
			newLine = "# " + k + "="
		}
		idx := -1
		for i, l := range lines { // сначала действующая строка, потом закомментированная
			if key, commented := keyOf(l); key == k && !commented {
				idx = i
				break
			}
		}
		if idx < 0 && v != "-" {
			for i, l := range lines {
				if key, commented := keyOf(l); key == k && commented {
					idx = i
					break
				}
			}
		}
		switch {
		case idx >= 0:
			lines[idx] = newLine
		case v != "-":
			lines = append(lines, newLine)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
