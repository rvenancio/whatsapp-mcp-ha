package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type headerRT struct {
	token string
	base  http.RoundTripper
}

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	return h.base.RoundTrip(r)
}

type env struct {
	cfg   *Config
	wa    *fakeWA
	store *Store
	srv   *httptest.Server
}

func newEnv(t *testing.T, mutate func(*Config)) *env {
	e, _ := newEnvAuth(t, mutate)
	return e
}

// newEnvAuth also returns the authenticator, for tests that rotate tokens.
func newEnvAuth(t *testing.T, mutate func(*Config)) (*env, *Authenticator) {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.EnsureSendToken(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(cfg.OutboxDir(), 0o755)
	wa := &fakeWA{status: Status{State: StateConnected, Paired: true, Connected: true}, contacts: map[string]string{"333333333@s.whatsapp.net": "Bob Builder"}}
	store := testStore(t)
	seed(t, store)
	auth := NewAuthenticator(cfg.AuthToken, quietLog())
	tools := &Tools{cfg: cfg, store: store, wa: wa, auth: auth, sends: NewSendLimiter(cfg.SendPerMinute, cfg.SendPerDay)}
	var sendServer *mcp.Server
	if cfg.SendActive() {
		auth.SetSendToken(cfg.SendToken)
		sendServer = NewMCPServer(tools, true)
	}
	auth.SetInactiveSendToken(cfg.InactiveSendToken)
	srv := httptest.NewServer(NewMCPHandler(NewMCPServer(tools, false), sendServer, auth))
	t.Cleanup(srv.Close)
	return &env{cfg: cfg, wa: wa, store: store, srv: srv}, auth
}

func (e *env) session(t *testing.T, token string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   e.srv.URL + MCPPath,
		HTTPClient: &http.Client{Transport: headerRT{token: token, base: http.DefaultTransport}, Timeout: 10 * time.Second},
		MaxRetries: -1,
	}, nil)
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(raw, &out)
	}
	return res, out
}

func errText(res *mcp.CallToolResult) string {
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestRoutesAndAuth(t *testing.T) {
	e := newEnv(t, nil)
	get := func(path, auth string) int {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/healthz", ""); c != 200 {
		t.Fatalf("healthz %d", c)
	}
	for _, p := range []string{"/", "/api/status", "/api/qr.png", "/app.js", "/mcp/../api/token", "/debug/pprof/"} {
		if c := get(p, "Bearer "+e.cfg.AuthToken); c != 404 && c != 401 {
			t.Errorf("%s: %d, want 404", p, c)
		}
	}
	post := func(auth string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("CF-Connecting-IP", "203.0.113.9")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, bad := range []string{"", "Bearer", "Bearer ", "Basic " + e.cfg.AuthToken, "Bearer " + e.cfg.AuthToken + "x", e.cfg.AuthToken} {
		if c := post(bad); c != 401 {
			t.Errorf("auth %q: %d, want 401", bad, c)
		}
	}
	if c := post("bearer " + e.cfg.AuthToken); c == 401 || c == 429 {
		t.Errorf("valid token rejected: %d", c)
	}
}

func TestAuthMethodNone(t *testing.T) {
	read := NewMCPServer(&Tools{cfg: testConfig(t), store: testStore(t), wa: &fakeWA{}, sends: NewSendLimiter(6, 50)}, false)
	none := NewAuthenticator("ignored", quietLog())
	none.UseNone(false)
	noneSrv := httptest.NewServer(NewMCPHandler(read, nil, none))
	t.Cleanup(noneSrv.Close)
	resp, err := http.Post(noneSrv.URL+MCPPath, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		t.Fatalf("none mode rejected a caller: %d", resp.StatusCode)
	}
}

func TestFailedAuthIsRateLimited(t *testing.T) {
	a := NewAuthenticator("secret-secret-secret-secret-secret", quietLog())
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(auth, cf string) int {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.RemoteAddr = "10.0.0.5:1234"
		req.Header.Set("CF-Connecting-IP", cf)
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < 10; i++ {
		if c := do("Bearer wrong", "198.51.100.1"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := do("Bearer wrong", "198.51.100.1"); c != 429 {
		t.Fatalf("11th failure: %d, want 429", c)
	}
	// Another caller behind the same tunnel connector is not locked out.
	if c := do("Bearer secret-secret-secret-secret-secret", "198.51.100.2"); c != 204 {
		t.Fatalf("other client: %d", c)
	}
	now = now.Add(time.Minute)
	if c := do("Bearer secret-secret-secret-secret-secret", "198.51.100.1"); c != 204 {
		t.Fatalf("after cool-down: %d", c)
	}
}

func TestMCPWrongTokenCannotConnect(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.session(t, "nope-nope-nope-nope-nope-nope-nope"); err == nil {
		t.Fatal("connected with a wrong token")
	}
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

func TestToolRegistration(t *testing.T) {
	readTools := "get_chat,get_contact_chats,get_direct_chat_by_contact,get_last_interaction,get_message_context,get_status,list_chats,list_messages,search_contacts"
	e := newEnv(t, nil)
	// The read-only token (the existing auth_token) never sees the send tools.
	cs, err := e.session(t, e.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if got := strings.Join(toolNames(t, cs), ","); got != readTools {
		t.Fatalf("read token tools:\n got %s\nwant %s", got, readTools)
	}
	// The send token gets the read tools plus the send tools.
	cs2, err := e.session(t, e.cfg.SendToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	want := readTools + ",send_audio_message,send_file,send_message"
	if got := strings.Join(toolNames(t, cs2), ","); got != want {
		t.Fatalf("send token tools:\n got %s\nwant %s", got, want)
	}

	// enable_send off: no token sees send tools, and the send token is not accepted at all.
	ro := newEnv(t, func(c *Config) { c.EnableSend = false; c.AllowMediaDownload = true })
	cs3, err := ro.session(t, ro.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs3.Close()
	names := strings.Join(toolNames(t, cs3), ",")
	if strings.Contains(names, "send_") || !strings.Contains(names, "download_media") {
		t.Fatalf("read-only tools: %s", names)
	}
	if _, err := ro.session(t, ro.cfg.SendToken); err == nil {
		t.Fatal("send token accepted while enable_send is off")
	}
}

func TestSendToolWithReadTokenIsToolError(t *testing.T) {
	e := newEnv(t, nil)
	cs, err := e.session(t, e.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// More attempts than the failed-auth limit (10): none of them may count as an authentication failure.
	for i := 0; i < 15; i++ {
		for _, name := range []string{"send_message", "send_file", "send_audio_message"} {
			res, _ := call(t, cs, name, map[string]any{"recipient": "351900000000", "message": "x", "media_path": "a.png"})
			if !res.IsError || !strings.Contains(errText(res), "send_token") {
				t.Fatalf("%s with the read token: %s", name, errText(res))
			}
		}
	}
	if len(e.wa.sent) != 0 || len(e.wa.files) != 0 {
		t.Fatal("sent with the read token")
	}
	if _, out := call(t, cs, "get_status", nil); out["send_allowed_for_this_token"] != false || out["send_enabled"] != true {
		t.Fatalf("status: %v", out)
	}
	// The same client can still use read tools: it was never rate limited.
	if res, _ := call(t, cs, "list_chats", nil); res.IsError {
		t.Fatalf("read token locked out: %s", errText(res))
	}
}

func TestEmptyAllowedRecipientsRefusesSends(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowedRecipients = nil })
	os.WriteFile(filepath.Join(e.cfg.OutboxDir(), "pic.png"), pngBytes(), 0o644)
	cs, err := e.session(t, e.cfg.SendToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for name, args := range map[string]map[string]any{
		"send_message": {"recipient": "351900000000", "message": "hi"},
		"send_file":    {"recipient": "351900000000", "media_path": "pic.png"},
	} {
		res, _ := call(t, cs, name, args)
		if !res.IsError || !strings.Contains(errText(res), "allowed_recipients is empty") {
			t.Fatalf("%s with an empty allowlist: %s", name, errText(res))
		}
	}
	if len(e.wa.sent) != 0 || len(e.wa.files) != 0 {
		t.Fatal("sent with an empty allowlist")
	}
}

func TestAuthenticatorRoles(t *testing.T) {
	a := NewAuthenticator("read-token-0123456789abcdefghijklmn", quietLog())
	if a.Role("Bearer send-token-0123456789abcdefghijklmn") != RoleNone {
		t.Fatal("send token accepted before it was set")
	}
	a.SetSendToken("send-token-0123456789abcdefghijklmn")
	for h, want := range map[string]Role{
		"Bearer read-token-0123456789abcdefghijklmn": RoleRead, "Bearer send-token-0123456789abcdefghijklmn": RoleSend,
		"Bearer ": RoleNone, "": RoleNone, "Basic send-token-0123456789abcdefghijklmn": RoleNone, "Bearer x": RoleNone,
	} {
		if got := a.Role(h); got != want {
			t.Errorf("%q: %v, want %v", h, got, want)
		}
	}
	a.SetSendToken("")
	if a.Role("Bearer send-token-0123456789abcdefghijklmn") != RoleNone {
		t.Fatal("send token still accepted after it was cleared")
	}
}

func TestReadTools(t *testing.T) {
	e := newEnv(t, nil)
	cs, err := e.session(t, e.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	_, st := call(t, cs, "get_status", nil)
	if st["state"] != "connected" || st["send_enabled"] != true {
		t.Fatalf("status: %v", st)
	}
	_, out := call(t, cs, "list_messages", map[string]any{"query": "hello"})
	if msgs := out["messages"].([]any); len(msgs) != 1 || out["context"] == nil ||
		msgs[0].(map[string]any)["untrusted"].(map[string]any)["content"] != "hello there" || msgs[0].(map[string]any)["content"] != nil {
		t.Fatalf("list_messages: %v", out)
	}
	_, out = call(t, cs, "list_chats", map[string]any{"limit": 1})
	if len(out["chats"].([]any)) != 1 {
		t.Fatalf("list_chats: %v", out)
	}
	_, out = call(t, cs, "search_contacts", map[string]any{"query": "bob"})
	if c := out["contacts"].([]any); len(c) != 1 || c[0].(map[string]any)["phone_number"] != "333333333" || c[0].(map[string]any)["untrusted"].(map[string]any)["name"] != "Bob Builder" {
		t.Fatalf("search_contacts: %v", out)
	}
	_, out = call(t, cs, "search_contacts", map[string]any{"query": "alice"})
	if len(out["contacts"].([]any)) != 1 {
		t.Fatalf("search_contacts from chats: %v", out)
	}
	_, out = call(t, cs, "get_chat", map[string]any{"chat_jid": "123-456@g.us"})
	if out["untrusted"].(map[string]any)["name"] != "Family" {
		t.Fatalf("get_chat: %v", out)
	}
	_, out = call(t, cs, "get_direct_chat_by_contact", map[string]any{"sender_phone_number": "+111111111"})
	if out["untrusted"].(map[string]any)["name"] != "Alice" {
		t.Fatalf("get_direct_chat_by_contact: %v", out)
	}
	_, out = call(t, cs, "get_message_context", map[string]any{"message_id": "A2", "chat_jid": "111111111@s.whatsapp.net"})
	if out["message"].(map[string]any)["id"] != "A2" {
		t.Fatalf("get_message_context: %v", out)
	}
	_, out = call(t, cs, "get_last_interaction", map[string]any{"jid": "111111111@s.whatsapp.net"})
	if out["id"] != "G1" {
		t.Fatalf("get_last_interaction: %v", out)
	}
	_, out = call(t, cs, "get_contact_chats", map[string]any{"jid": "111111111@s.whatsapp.net"})
	if len(out["chats"].([]any)) != 2 {
		t.Fatalf("get_contact_chats: %v", out)
	}
	res, _ := call(t, cs, "get_chat", map[string]any{"chat_jid": "nope@g.us"})
	if !res.IsError {
		t.Fatal("missing chat not an error")
	}
	res, _ = call(t, cs, "list_messages", map[string]any{"after": "yesterday"})
	if !res.IsError {
		t.Fatal("bad date accepted")
	}
}

func TestSendTools(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.SendPerMinute = 3 })
	os.WriteFile(filepath.Join(e.cfg.OutboxDir(), "pic.png"), pngBytes(), 0o644)
	os.WriteFile(filepath.Join(e.cfg.MediaDir, "secret.txt"), []byte("nope"), 0o644)
	cs, err := e.session(t, e.cfg.SendToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, out := call(t, cs, "send_message", map[string]any{"recipient": "+351900000000", "message": "hi"})
	if res.IsError || out["message_id"] != "SENT1" || e.wa.sent[0] != "351900000000@s.whatsapp.net|hi" {
		t.Fatalf("send_message: %v %v %s", out, e.wa.sent, errText(res))
	}
	for _, bad := range []map[string]any{
		{"recipient": "status@broadcast", "message": "x"},
		{"recipient": "123@newsletter", "message": "x"},
		{"recipient": "12ab", "message": "x"},
		{"recipient": "351900000000", "message": "   "},
		{"recipient": "351900000000", "message": strings.Repeat("a", maxTextLength+1)},
	} {
		if res, _ := call(t, cs, "send_message", bad); !res.IsError {
			t.Errorf("accepted %v", bad)
		}
	}
	res, _ = call(t, cs, "send_file", map[string]any{"recipient": "123-456@g.us", "media_path": "pic.png", "caption": "c"})
	if res.IsError || len(e.wa.files) != 1 || e.wa.files[0].Kind != KindImage {
		t.Fatalf("send_file: %s", errText(res))
	}
	res, _ = call(t, cs, "send_file", map[string]any{"recipient": "123-456@g.us", "media_path": "../secret.txt"})
	if !res.IsError || !strings.Contains(errText(res), "outbox") {
		t.Fatalf("path traversal: %s", errText(res))
	}
	res, _ = call(t, cs, "send_audio_message", map[string]any{"recipient": "123-456@g.us", "media_path": "pic.png"})
	if !res.IsError {
		t.Fatal("png accepted as voice message")
	}
	// Third successful send hits the limit of 3 per minute (2 used so far).
	call(t, cs, "send_message", map[string]any{"recipient": "351900000000", "message": "3"})
	res, _ = call(t, cs, "send_message", map[string]any{"recipient": "351900000000", "message": "4"})
	if !res.IsError || !strings.Contains(errText(res), "rate limit") {
		t.Fatalf("rate limit: %s", errText(res))
	}
}

func TestSendLimiterDaily(t *testing.T) {
	l := NewSendLimiter(60, 2)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	if l.Allow() != nil || l.Allow() != nil {
		t.Fatal("first sends refused")
	}
	if l.Allow() == nil {
		t.Fatal("daily limit not applied")
	}
	now = now.Add(25 * time.Hour)
	if l.Allow() != nil {
		t.Fatal("daily window did not roll")
	}
}

func TestDownloadMedia(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowMediaDownload = true })
	e.wa.media = []byte("imagedata")
	// A planted symlink in the downloads folder must not redirect the write.
	os.MkdirAll(e.cfg.DownloadDir(), 0o755)
	outside := filepath.Join(t.TempDir(), "victim")
	os.Mkdir(outside, 0o755)
	os.Symlink(outside, filepath.Join(e.cfg.DownloadDir(), "123-456"))
	cs, err := e.session(t, e.cfg.AuthToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, _ := call(t, cs, "download_media", map[string]any{"message_id": "G2", "chat_jid": "123-456@g.us"})
	if !res.IsError {
		t.Fatal("download followed a symlink out of the downloads folder")
	}
	os.Remove(filepath.Join(e.cfg.DownloadDir(), "123-456"))
	res, out := call(t, cs, "download_media", map[string]any{"message_id": "G2", "chat_jid": "123-456@g.us"})
	if res.IsError {
		t.Fatal(errText(res))
	}
	p := out["file_path"].(string)
	if !strings.HasPrefix(p, e.cfg.DownloadDir()) {
		t.Fatalf("written outside downloads: %s", p)
	}
	if b, _ := os.ReadFile(p); string(b) != "imagedata" {
		t.Fatal("content mismatch")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("file written through the symlink")
	}
	e.wa.media = make([]byte, 2<<20)
	res, _ = call(t, cs, "download_media", map[string]any{"message_id": "G2", "chat_jid": "123-456@g.us"})
	if !res.IsError {
		t.Fatal("oversized media accepted")
	}
}

func TestParseRecipient(t *testing.T) {
	ok := map[string]string{"351900000000": "351900000000@s.whatsapp.net", "+351900000000": "351900000000@s.whatsapp.net",
		"351900000000@s.whatsapp.net": "351900000000@s.whatsapp.net", "123-456@g.us": "123-456@g.us", "1234567@lid": "1234567@lid",
		"351900000000:3@s.whatsapp.net": "351900000000@s.whatsapp.net"}
	for in, want := range ok {
		j, err := ParseRecipient(in)
		if err != nil || j.String() != want {
			t.Errorf("%q -> %v %v", in, j, err)
		}
	}
	for _, bad := range []string{"", "abc", "123", "status@broadcast", "1@newsletter", "x@s.whatsapp.net", "1@bot", "1@msgr"} {
		if _, err := ParseRecipient(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAllowedRecipientsEnforced(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowedRecipients = map[string]bool{"351900000000@s.whatsapp.net": true} })
	os.WriteFile(filepath.Join(e.cfg.OutboxDir(), "pic.png"), pngBytes(), 0o644)
	cs, err := e.session(t, e.cfg.SendToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if res, _ := call(t, cs, "send_message", map[string]any{"recipient": "+351900000000", "message": "ok"}); res.IsError {
		t.Fatalf("allowed recipient refused: %s", errText(res))
	}
	res, _ := call(t, cs, "send_message", map[string]any{"recipient": "351911111111", "message": "no"})
	if !res.IsError || !strings.Contains(errText(res), "allowed_recipients") {
		t.Fatalf("other recipient: %s", errText(res))
	}
	if res, _ := call(t, cs, "send_file", map[string]any{"recipient": "123-456@g.us", "media_path": "pic.png"}); !res.IsError {
		t.Fatal("file to a non-allowed group sent")
	}
	if len(e.wa.sent) != 1 || len(e.wa.files) != 0 {
		t.Fatalf("sent %v files %d", e.wa.sent, len(e.wa.files))
	}
	_, st := call(t, cs, "get_status", nil)
	if st["send_recipients_restricted"] != true {
		t.Fatalf("status: %v", st)
	}
}

func TestPageCapped(t *testing.T) {
	if pageOf(-5) != 0 || pageOf(10) != 10 || pageOf(1<<40) != maxPage {
		t.Fatal("pageOf")
	}
}

func TestAuthEvictsOldestAndLimitsLogs(t *testing.T) {
	var buf strings.Builder
	a := NewAuthenticator("Zq3vR8kP1xW7mN4tY6bC9dF2gH5jL0sA", slog.New(slog.NewTextHandler(&buf, nil)))
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		a.recordFailure("attacker")
	}
	if !a.limited("attacker") {
		t.Fatal("attacker not limited")
	}
	// Filling the table with fresh keys evicts the oldest entries one by one and keeps the attacker's limit
	// as long as the attacker is not the least recently seen.
	now = now.Add(time.Second)
	a.recordFailure("attacker")
	for i := 0; i < maxFailEntries; i++ {
		now = now.Add(time.Microsecond)
		a.recordFailure("k" + strconv.Itoa(i))
		if i == 10 {
			a.recordFailure("attacker") // keeps hammering, so it is never the oldest
		}
	}
	if len(a.failures) > maxFailEntries {
		t.Fatalf("table grew to %d", len(a.failures))
	}
	if _, ok := a.failures["k0"]; ok {
		t.Fatal("oldest key not evicted")
	}
	if !a.limited("attacker") {
		t.Fatal("eviction cleared the attacker's limit")
	}
	for i := 0; i < 1000; i++ {
		a.logFailure()
	}
	if n := strings.Count(buf.String(), "rejected MCP request"); n > 5 {
		t.Fatalf("%d failed-auth log lines, want at most 5", n)
	}
	if strings.Contains(buf.String(), "client=") {
		t.Fatalf("failed-auth log names the client: %s", buf.String())
	}
}
