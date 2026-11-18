package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	data := "\xEF\xBB\xBF# комментарий\r\nTZ=Asia/Tokyo\r\nBOT_TOKEN = 123:abc-DEF \r\n# ADDR=1.2.3.4:5\r\nALLOWED_USERS=\"1, @me\"\r\nexport WEBAPP_URL=https://x/\r\nmusor\r\n"
	got := parseEnvFile([]byte(data))
	want := map[string]string{"TZ": "Asia/Tokyo", "BOT_TOKEN": "123:abc-DEF", "ALLOWED_USERS": "1, @me", "WEBAPP_URL": "https://x/"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestUpdateEnvFile(t *testing.T) {
	out := string(updateEnvFile([]byte(envTemplate), map[string]string{
		"TZ": "Asia/Tokyo", "ADDR": "127.0.0.1:9000", "BOT_TOKEN": "-", "NEW_KEY": "x",
	}, []string{"TZ", "ADDR", "BOT_TOKEN", "NEW_KEY"}))
	for _, want := range []string{"\nTZ=Asia/Tokyo\n", "\nADDR=127.0.0.1:9000\n", "\n# BOT_TOKEN=\n", "\nNEW_KEY=x\n", "# После правки"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "# ADDR=") {
		t.Error("commented ADDR should be replaced, not duplicated")
	}
	// повтор не плодит строки
	again := string(updateEnvFile([]byte(out), map[string]string{"TZ": "UTC"}, []string{"TZ"}))
	if strings.Count(again, "TZ=") != 1 || !strings.Contains(again, "\nTZ=UTC\n") {
		t.Errorf("second update:\n%s", again)
	}
	if got := parseEnvFile([]byte(again)); got["BOT_TOKEN"] != "" || got["ADDR"] != "127.0.0.1:9000" {
		t.Errorf("parse after update: %v", got)
	}
}

func TestLoadConfigFromEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifelog.env")
	os.WriteFile(path, []byte("TZ=Asia/Tokyo\nBOT_TOKEN=1:x\nALLOWED_USERS=42,@me\nWEBAPP_URL=https://app.example/\nADDR=127.0.0.1:9999\n"), 0o600)
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	t.Setenv("ENV_FILE", path)
	t.Setenv("ADDR", "127.0.0.1:7777") // окружение процесса важнее файла

	c, err := loadConfig(false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Loc.String() != "Asia/Tokyo" || c.BotToken != "1:x" || c.WebAppURL != "https://app.example/" ||
		c.Addr != "127.0.0.1:7777" || !c.Allowed.Allowed(42, "") || !c.Allowed.Allowed(1, "ME") {
		t.Fatalf("config: %+v", c)
	}

	os.WriteFile(path, []byte("BOT_TOKEN=1:x\n"), 0o600)
	if _, err := loadConfig(false); err == nil {
		t.Error("BOT_TOKEN without ALLOWED_USERS must fail")
	}
	os.WriteFile(path, []byte("BOT_TOKEN=\nALLOWED_USERS=\n"), 0o600)
	if c, err := loadConfig(false); err != nil || c.BotToken != "" {
		t.Errorf("empty values = defaults: %+v %v", c, err)
	}
}
