package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfigFromOptions(t *testing.T) {
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	os.WriteFile(opts, []byte(`{"auth_token":"","enable_send":false,"send_rate_per_minute":3,"send_daily_limit":20,"allow_media_download":true,"max_media_mb":8,"log_level":"warning"}`), 0o600)
	cfg, err := LoadConfig(opts, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AddonMode || cfg.PageListen != ":8121" || cfg.EnableSend || cfg.SendPerMinute != 3 || !cfg.AllowMediaDownload || cfg.MaxMediaMB != 8 {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestLoadConfigFromEnvDefaults(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_DATA_DIR": "/x"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AddonMode || cfg.PageListen != "" || cfg.EnableSend || cfg.SendToken != "" || cfg.AllowMediaDownload || cfg.DataDir != "/x" || cfg.Listen != ":8120" {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
}

func TestShortTokenRejected(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": "short"}))
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("want short token error, got %v", err)
	}
	_, err = LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": strings.Repeat("a", 31) + " b"}))
	if err == nil {
		t.Fatal("token with a space accepted")
	}
}

func TestLimitsValidated(t *testing.T) {
	for _, env := range []map[string]string{
		{"WA_MCP_SEND_RATE_PER_MINUTE": "0"}, {"WA_MCP_SEND_DAILY_LIMIT": "0"}, {"WA_MCP_MAX_MEDIA_MB": "500"}, {"WA_MCP_ENABLE_SEND": "maybe"},
	} {
		if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(env)); err == nil {
			t.Fatalf("accepted %v", env)
		}
	}
}

func TestGeneratedTokenPersisted(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	if err := cfg.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	if !cfg.TokenGenerated || len(cfg.AuthToken) < MinTokenLength {
		t.Fatalf("bad generated token %q", cfg.AuthToken)
	}
	st, _ := os.Stat(filepath.Join(cfg.DataDir, "auth_token"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", st.Mode())
	}
	again := &Config{DataDir: cfg.DataDir}
	again.EnsureToken()
	if again.AuthToken != cfg.AuthToken {
		t.Fatal("generated token not reused")
	}
	own := &Config{DataDir: t.TempDir(), AuthToken: strings.Repeat("x", 40)}
	own.EnsureToken()
	if own.TokenGenerated || own.AuthToken != strings.Repeat("x", 40) {
		t.Fatal("configured token replaced")
	}
}

func TestWeakTokensRejected(t *testing.T) {
	for _, tok := range []string{strings.Repeat("a", 40), strings.Repeat("abc", 15), strings.Repeat("01234567", 5), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaabcd"} {
		if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": tok})); err == nil {
			t.Errorf("weak token %q accepted", tok)
		}
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"), envMap(map[string]string{"WA_MCP_TOKEN": "Zq3vR8kP1xW7mN4tY6bC9dF2gH5jL0sA"})); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedRecipientsAndRetention(t *testing.T) {
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	os.WriteFile(opts, []byte(`{"allowed_recipients":["+351900000000","123-456@g.us",""],"retention_days":30}`), 0o600)
	cfg, err := LoadConfig(opts, envMap(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AllowedRecipients) != 2 || !cfg.AllowedRecipients["351900000000@s.whatsapp.net"] || cfg.RetentionDays != 30 || cfg.SendPerDay != 50 {
		t.Fatalf("%+v", cfg)
	}
	os.WriteFile(opts, []byte(`{"allowed_recipients":["status@broadcast"]}`), 0o600)
	if _, err := LoadConfig(opts, envMap(nil)); err == nil {
		t.Fatal("invalid allowed recipient accepted")
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.json"), envMap(map[string]string{"WA_MCP_RETENTION_DAYS": "-1"})); err == nil {
		t.Fatal("negative retention accepted")
	}
	cfg, _ = LoadConfig(filepath.Join(dir, "missing.json"), envMap(map[string]string{"WA_MCP_ALLOWED_RECIPIENTS": "351911111111,1-2@g.us"}))
	if len(cfg.AllowedRecipients) != 2 {
		t.Fatalf("env allowlist: %+v", cfg.AllowedRecipients)
	}
}

func TestSendTokenRules(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	read := "read-token-0123456789abcdefghijklmn"
	send := "send-token-0123456789abcdefghijklmn"
	if _, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_TOKEN": read, "WA_MCP_SEND_TOKEN": read})); err == nil ||
		!strings.Contains(err.Error(), "different") {
		t.Fatalf("equal tokens: %v", err)
	}
	if _, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_SEND_TOKEN": "short"})); err == nil ||
		!strings.Contains(err.Error(), "send_token") {
		t.Fatalf("short send token: %v", err)
	}
	if _, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_SEND_TOKEN": strings.Repeat("ab", 20)})); err == nil {
		t.Fatal("predictable send token accepted")
	}
	cfg, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_TOKEN": read, "WA_MCP_SEND_TOKEN": send}))
	if err != nil || cfg.SendActive() {
		t.Fatalf("send must stay off without enable_send: %v %+v", err, cfg)
	}
	cfg, err = LoadConfig(missing, envMap(map[string]string{"WA_MCP_TOKEN": read, "WA_MCP_SEND_TOKEN": send, "WA_MCP_ENABLE_SEND": "true"}))
	if err != nil || !cfg.SendActive() {
		t.Fatalf("send should be active: %v", err)
	}
	// A configured send token equal to a generated read token is refused when the read token is loaded.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, authTokenFile), []byte(send+"\n"), 0o600)
	c := &Config{DataDir: dir, SendToken: send, EnableSend: true}
	if err := c.EnsureToken(); err != nil || c.AuthToken == send {
		t.Fatalf("stored read token equal to send_token must be replaced: %v", err)
	}
}

func TestGeneratedSendToken(t *testing.T) {
	dir := t.TempDir()
	c := &Config{DataDir: dir}
	if err := c.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureSendToken(); err != nil || c.SendToken != "" || c.SendTokenGenerated {
		t.Fatalf("send token generated while enable_send is off: %v %q", err, c.SendToken)
	}
	if _, err := os.Stat(filepath.Join(dir, sendTokenFile)); err == nil {
		t.Fatal("send_token file written while enable_send is off")
	}
	c.EnableSend = true
	if err := c.EnsureSendToken(); err != nil || !c.SendTokenGenerated || len(c.SendToken) < MinTokenLength || c.SendToken == c.AuthToken {
		t.Fatalf("generated send token: %v %+v", err, c)
	}
	st, _ := os.Stat(filepath.Join(dir, sendTokenFile))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("send_token mode %v", st.Mode())
	}
	again := &Config{DataDir: dir, EnableSend: true, AuthToken: c.AuthToken}
	if err := again.EnsureSendToken(); err != nil || again.SendToken != c.SendToken {
		t.Fatal("generated send token not persisted")
	}
	if !again.SendActive() {
		t.Fatal("generated send token does not activate sending")
	}
}

func TestOldOptionsKeepReadTokenAndDisableSendUntilConfigured(t *testing.T) {
	// A 0.1.0 options.json: enable_send true (old default), no send_token, empty allowed_recipients.
	dir := t.TempDir()
	opts := filepath.Join(dir, "options.json")
	os.WriteFile(opts, []byte(`{"auth_token":"read-token-0123456789abcdefghijklmn","enable_send":true,"send_rate_per_minute":6,"send_daily_limit":50,"allowed_recipients":[]}`), 0o600)
	cfg, err := LoadConfig(opts, envMap(map[string]string{"WA_MCP_DATA_DIR": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthToken != "read-token-0123456789abcdefghijklmn" || len(cfg.AllowedRecipients) != 0 {
		t.Fatalf("%+v", cfg)
	}
}

func TestAuthMethodSelection(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	cfg, err := LoadConfig(missing, envMap(nil))
	if err != nil || cfg.AuthMethod != AuthMethodAPIKey {
		t.Fatalf("default method: %v %+v", err, cfg)
	}
	cfg, err = LoadConfig(missing, envMap(map[string]string{"WA_MCP_AUTH_METHOD": "none", "WA_MCP_TOKEN": "short"}))
	if err != nil || cfg.AuthMethod != AuthMethodNone {
		t.Fatalf("none should ignore a short token: %v %+v", err, cfg)
	}
	dir := t.TempDir()
	c := &Config{DataDir: dir, AuthMethod: AuthMethodNone, EnableSend: true}
	if err := c.EnsureToken(); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureSendToken(); err != nil || !c.SendActive() {
		t.Fatalf("none + enable_send: %v %+v", err, c)
	}
	if _, err := os.Stat(filepath.Join(dir, authTokenFile)); err == nil {
		t.Fatal("none mode wrote an auth token")
	}
	if _, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_AUTH_METHOD": "cloudflare_jwt"})); err == nil {
		t.Fatal("removed cloudflare_jwt method was accepted")
	}
	if _, err := LoadConfig(missing, envMap(map[string]string{"WA_MCP_AUTH_METHOD": "basic"})); err == nil {
		t.Fatal("unknown auth_method accepted")
	}
}
