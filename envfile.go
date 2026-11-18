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
# ALLOW_NO_AUTH=
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

func parseEnvLine(line string) (key, value string, commented bool) {
	line = strings.TrimSpace(line)
	commented = strings.HasPrefix(line, "#")
	line = strings.TrimSpace(strings.TrimPrefix(line, "#"))
	line = strings.TrimPrefix(line, "export ")
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", commented
	}
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		value = value[1 : len(value)-1]
	}
	return key, value, commented
}

func parseEnvFile(data []byte) map[string]string {
	out := map[string]string{}
	for _, line := range envLines(data) {
		if k, v, commented := parseEnvLine(line); k != "" && !commented {
			out[k] = v
		}
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
	for _, k := range order {
		v, ok := set[k]
		if !ok {
			continue
		}
		newLine, disabled := k+"="+v, "# "+k+"="
		if v == "-" {
			newLine = disabled
		}
		first, commentedLine := -1, -1
		for i, line := range lines {
			key, _, commented := parseEnvLine(line)
			if key != k {
				continue
			}
			switch {
			case commented:
				if commentedLine < 0 {
					commentedLine = i
				}
			case first < 0:
				first = i
			default:
				lines[i] = disabled
			}
		}
		switch {
		case first >= 0:
			lines[first] = newLine
		case v != "-" && commentedLine >= 0:
			lines[commentedLine] = newLine
		case v != "-":
			lines = append(lines, newLine)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
