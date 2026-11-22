// Command whatsapp-mcp links a personal WhatsApp account as a device (whatsmeow, like WhatsApp Web)
// and serves it as an MCP server over streamable HTTP.
//
// The message store and tool set are derived from lharries/whatsapp-mcp (MIT).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// exitConfigError tells the s6 finish script to stop the add-on instead of restarting it.
const exitConfigError = 2

type configError struct{ error }

func main() {
	// The masking handler is installed before the configuration is read, so even a configuration error
	// cannot print someone's phone number. The level is set once the configuration is known.
	level := new(slog.LevelVar)
	slog.SetDefault(slog.New(newRedactingHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))))
	if err := run(level); err != nil {
		slog.Error("fatal", "error", err)
		var ce configError
		if errors.As(err, &ce) {
			os.Exit(exitConfigError)
		}
		os.Exit(1)
	}
}

func run(level *slog.LevelVar) error {
	cfg, err := LoadConfig("/data/options.json", os.Getenv)
	if err != nil {
		return configError{err}
	}
	// Every line goes through the redacting handler: other people's JIDs and phone numbers are masked.
	level.Set(cfg.LogLevel)
	log := slog.Default()

	// Session keys, messages and the generated token: owner-only files.
	syscall.Umask(0o077)
	// Also tightens files left by older versions: data folder 0700, session/message/token files 0600,
	// downloaded media 0600 in 0700 folders.
	if n, err := tightenDataDir(cfg.DataDir); err != nil {
		log.Warn("cannot restrict the data folder permissions", "error", err)
	} else if n > 0 {
		log.Info("restricted data folder permissions", "changed", n)
	}
	if n, err := tightenTree(cfg.DownloadDir()); err != nil {
		log.Warn("cannot restrict the downloads folder permissions", "error", err)
	} else if n > 0 {
		log.Info("restricted downloads folder permissions", "changed", n)
	}
	if err := os.MkdirAll(cfg.OutboxDir(), 0o750); err != nil {
		log.Warn("cannot create the outbox folder; send_file will not work", "error", err)
	}
	if err := cfg.EnsureToken(); err != nil {
		return configError{err}
	}
	if err := cfg.EnsureSendToken(); err != nil {
		return configError{err}
	}
	switch cfg.AuthMethod {
	case AuthMethodNone:
		log.Warn("auth_method is none: /mcp has no authentication. Anyone who can reach it can use this WhatsApp account. This is not recommended")
	}
	if !cfg.usesAPIKey() && (cfg.AuthToken != "" || cfg.SendToken != "") {
		log.Info("auth_token and send_token are ignored because auth_method is not api_key")
	}
	if cfg.SendTokenGenerated {
		log.Info("no send_token configured: using a generated send token. Open the add-on page to see it (only the client that may send should get it)")
	}
	switch {
	case cfg.usesAPIKey() && !cfg.EnableSend && cfg.SendToken != "":
		log.Info("send_token is set but enable_send is off: send tools are off and send_token is not accepted")
	case cfg.SendActive() && len(cfg.AllowedRecipients) == 0:
		log.Warn("send tools are on but allowed_recipients is empty: every send will be refused")
	}
	if cfg.TokenGenerated {
		if cfg.AddonMode {
			log.Info("no auth_token configured: using a generated token. Open the add-on page (Open Web UI) to see it, or set auth_token in the configuration")
		} else {
			log.Info("no WA_MCP_TOKEN set: using a generated token stored in the data folder (auth_token file)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(filepath.Join(cfg.DataDir, "messages.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	_ = os.Chmod(filepath.Join(cfg.DataDir, "messages.db"), 0o600)

	mgr, err := NewManager(ctx, cfg, store, log)
	if err != nil {
		return err
	}
	auth := NewAuthenticator(cfg.AuthToken, log)
	switch cfg.AuthMethod {
	case AuthMethodNone:
		auth.UseNone(cfg.SendActive())
	default:
		if cfg.SendActive() {
			auth.SetSendToken(cfg.SendToken)
		}
		auth.SetInactiveSendToken(cfg.InactiveSendToken)
	}
	var sendServer *mcp.Server
	tools := &Tools{cfg: cfg, store: store, wa: mgr, auth: auth, sends: NewSendLimiter(cfg.SendPerMinute, cfg.SendPerDay)}
	if cfg.SendActive() {
		sendServer = NewMCPServer(tools, true)
	}
	servers := []*http.Server{newHTTPServer(cfg.Listen, NewMCPHandler(NewMCPServer(tools, false), sendServer, auth))}
	if cfg.PageListen != "" {
		servers = append(servers, newHTTPServer(cfg.PageListen, NewPageHandler(cfg, store, mgr, auth)))
	}
	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}(s)
	}
	log.Info("WhatsApp MCP started", "version", Version, "mcp", cfg.Listen+MCPPath,
		"auth_method", cfg.AuthMethod, "send_tools", cfg.SendActive(), "media_download", cfg.AllowMediaDownload,
		"allowed_recipients", len(cfg.AllowedRecipients), "retention_days", cfg.RetentionDays)

	go store.RunRetention(ctx, cfg.RetentionDays, 6*time.Hour, log)

	done := make(chan struct{})
	if os.Getenv("WA_MCP_NO_CONNECT") == "1" {
		// Test hook (CI smoke test): serve HTTP without ever contacting WhatsApp.
		log.Warn("WA_MCP_NO_CONNECT=1: not connecting to WhatsApp")
		close(done)
	} else {
		go func() { mgr.Run(ctx); close(done) }()
	}

	select {
	case <-ctx.Done():
	case err = <-errCh:
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	select {
	case <-done:
	case <-shutdownCtx.Done():
	}
	return err
}
