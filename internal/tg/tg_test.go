package tg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "123456:TEST-token"

func initData(id int64, username string, at time.Time) string {
	return SignInitData(token, map[string]string{
		"auth_date": strconv.FormatInt(at.Unix(), 10),
		"query_id":  "AAH",
		"user":      `{"id":` + strconv.FormatInt(id, 10) + `,"first_name":"T","username":"` + username + `"}`,
		"signature": "abc",
	})
}

func TestValidateInitData(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := initData(42, "Tester", now.Add(-time.Minute))

	d, err := ValidateInitData(good, token, time.Hour, now)
	if err != nil || d.User.ID != 42 || d.User.Username != "Tester" || len(d.Hash) != 64 || !d.AuthDate.Equal(now.Add(-time.Minute)) {
		t.Fatalf("valid: %+v %v", d, err)
	}

	// Подмена id в подписанной строке.
	forged := strings.Replace(good, "%22id%22%3A42", "%22id%22%3A43", 1)
	if forged == good {
		t.Fatal("test did not modify id")
	}
	if _, err := ValidateInitData(forged, token, time.Hour, now); err != ErrBadSignature {
		t.Errorf("forged id: %v", err)
	}
	// Подпись чужим токеном.
	other := SignInitData("999:other", map[string]string{"auth_date": strconv.FormatInt(now.Unix(), 10), "user": `{"id":42}`})
	if _, err := ValidateInitData(other, token, time.Hour, now); err != ErrBadSignature {
		t.Errorf("other token: %v", err)
	}
	// Старая строка (повтор перехваченной).
	if _, err := ValidateInitData(initData(42, "x", now.Add(-2*time.Hour)), token, time.Hour, now); err != ErrExpired {
		t.Errorf("expired: %v", err)
	}
	for _, bad := range []string{"", "hash=zz", "user=%7B%7D", "%%%"} {
		if _, err := ValidateInitData(bad, token, time.Hour, now); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestAllowlist(t *testing.T) {
	a := ParseAllowlist(SplitList("111, @Owner2 ;tester"))
	cases := []struct {
		id   int64
		name string
		want bool
	}{
		{111, "", true},
		{222, "owner2", true}, // по username, без учёта регистра и @
		{333, "TESTER", true},
		{444, "nobody", false},
		{0, "", false},
	}
	for _, c := range cases {
		if got := a.Allowed(c.id, c.name); got != c.want {
			t.Errorf("Allowed(%d,%q)=%v", c.id, c.name, got)
		}
	}
	if !a.IsOwner(111) || a.IsOwner(222) {
		t.Error("owner = first entry")
	}
	if ParseAllowlist(nil).Allowed(1, "x") {
		t.Error("empty list allows nobody")
	}
}

func TestBotHandle(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["_method"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer api.Close()

	b := &Bot{Token: token, WebAppURL: "https://app.example.ts.net/", Allow: ParseAllowlist([]string{"111"}),
		APIBase: api.URL, Client: api.Client()}
	ctx := context.Background()
	msg := func(id int64, chatType string) *message {
		m := &message{Text: "/start"}
		m.Chat.ID, m.Chat.Type = id, chatType
		m.From = &struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		}{ID: id}
		return m
	}
	if err := b.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	b.Handle(ctx, msg(111, "private"))
	b.Handle(ctx, msg(222, "private"))
	b.Handle(ctx, msg(111, "group")) // в группах молчим

	var methods []string
	for _, m := range sent {
		methods = append(methods, m["_method"].(string))
	}
	if got := strings.Join(methods, ","); got != "deleteWebhook,setChatMenuButton,setMyCommands,sendMessage,sendMessage" {
		t.Fatalf("calls: %s", got)
	}
	allowed, denied := sent[3], sent[4]
	if !strings.Contains(toJSON(allowed["reply_markup"]), `"url":"https://app.example.ts.net/"`) {
		t.Errorf("allowed reply: %v", allowed)
	}
	if denied["text"] != NotAllowedText || denied["reply_markup"] != nil {
		t.Errorf("denied reply: %v", denied)
	}
}

func TestTokenNotInErrors(t *testing.T) {
	b := &Bot{Token: token, APIBase: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}}
	err := b.call(context.Background(), "getMe", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked or no error: %v", err)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestOwnerRequiresNumericFirstEntry(t *testing.T) {
	byName := ParseAllowlist([]string{"@fixture", "222"})
	if !byName.Allowed(111, "fixture") || byName.IsOwner(111) || byName.IsOwner(222) {
		t.Fatal("username даёт доступ, а владельцем может быть только первая числовая запись")
	}
	byID := ParseAllowlist([]string{"111", "@fixture"})
	if !byID.IsOwner(111) || byID.IsOwner(222) {
		t.Fatal("владелец определяется по первому ID")
	}
}
