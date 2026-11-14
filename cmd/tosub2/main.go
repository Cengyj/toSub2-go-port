package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/jshost"
	"tosub2-go-port/internal/logging"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

// dist contains the production static console. Build pipelines may replace
// these files with the React output; serving the UI never invokes Node.
//
//go:embed web/dist
var staticFiles embed.FS

func main() {
	if handled, code := jshost.WorkerMain(os.Args[1:], os.Stdin, os.Stdout); handled {
		os.Exit(code)
	}
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(2)
	}
	logger := logging.New()
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if handled, err := runCredentialImport(rootCtx, cfg, os.Stdout); handled {
		if err != nil {
			logger.Error("offline credential import failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := runPlatformService(cfg, logger); handled {
		if err != nil {
			logger.Error("service operation failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := runCLI(rootCtx, cfg, logger); handled {
		if err != nil {
			logger.Error("CLI operation failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if err := runConsole(rootCtx, cfg, logger, nil); err != nil {
		logger.Error("console service failed", "error", err)
		os.Exit(1)
	}
}

// runConsole shares cancellation between signals and Windows Service Control.
// ready is called only after bind, restore and startup.
func runConsole(parent context.Context, cfg config.Config, logger *slog.Logger, ready func()) error {
	rootCtx, cancelRoot := context.WithCancel(parent)
	defer cancelRoot()
	st, err := store.New(cfg.OutputRoot)
	if err != nil {
		return fmt.Errorf("store initialization failed: %w", err)
	}
	listener, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("HTTP listener initialization failed: %w", err)
	}
	defer listener.Close()
	manager := jobs.NewManager(rootCtx, cfg.MaxActiveJobs, st, logger)
	configureAccountOperationLease(cfg, manager)
	manager.SetRunner(makeJobRunner(cfg, manager, logger))
	if err := manager.Restore(rootCtx); err != nil {
		// Failed recovery must not replace original metadata with a partial snapshot.
		return fmt.Errorf("job metadata restore failed: %w", err)
	}
	consoleServer := console.NewServer(cfg, manager, staticFiles, logger)
	// Install the persisted automatic-repair policy before admitting new work.
	// Restored account records never resume an old authentication operation.
	manager.Start()
	monitorCtx, stopMonitor := context.WithCancel(rootCtx)
	defer stopMonitor()
	consoleServer.StartMonitor(monitorCtx)
	consoleServer.StartAccountInsights(rootCtx)
	consoleServer.StartAccountQuality(rootCtx)
	consoleServer.StartRecycleBin(rootCtx)
	// Foreground metadata reads allow 120 seconds per upstream request, and
	// uploads may process a batch sequentially. A global 60-second write deadline discarded valid
	// results before the handler could reply. Each outbound call remains
	// bounded; root cancellation also cancels foreground request handlers.
	server := &http.Server{
		Addr: cfg.Addr(), Handler: consoleServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return rootCtx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	logger.Info("toSub2 Go console listening", "addr", cfg.Addr(), "outputRoot", cfg.OutputRoot)
	if cfg.Host != "localhost" && (net.ParseIP(cfg.Host) == nil || !net.ParseIP(cfg.Host).IsLoopback()) {
		logger.Warn("console is listening on a non-loopback interface; use a trusted LAN and never expose it directly to the public Internet")
	}
	if ready != nil {
		ready()
	}
	var serveErr error
	select {
	case <-rootCtx.Done():
	case serveErr = <-serveDone:
		cancelRoot()
	}
	stopMonitor()
	timeout := time.Duration(cfg.ShutdownTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var closeErrors []error
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		closeErrors = append(closeErrors, fmt.Errorf("HTTP shutdown: %w", err))
	}
	if err := consoleServer.CloseContext(shutdownCtx); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("console resource shutdown: %w", err))
	}
	if err := manager.Shutdown(shutdownCtx); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("job manager shutdown: %w", err))
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		closeErrors = append(closeErrors, fmt.Errorf("HTTP server stopped: %w", serveErr))
	}
	logger.Info("toSub2 stopped")
	return errors.Join(closeErrors...)
}

func randomStrongPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("secure password generation failed: %w", err)
	}
	return "T2-" + base64.RawURLEncoding.EncodeToString(b) + "!aA9", nil
}

func refreshExistingExport(ctx context.Context, doer protocol.Doer, authBase, clientID, root, id, redirectURI string) error {
	_ = redirectURI
	path := filepath.Join(root, id, "sub2api-import-oauth.json")
	return refreshExistingFile(ctx, doer, authBase, clientID, path, "")
}

// updateRefreshedDocument operates on a validated private copy. The caller
// persists the accepted grant before applying optional metadata enrichment.
func updateRefreshedDocument(raw map[string]any, tok protocol.TokenSet, clientID string) map[string]any {
	// Read only the new grant before filling omitted tokens from disk. Its
	// access-token expiry and plan must not come from an older ID token.
	metadata := protocol.ExtractOAuthTokenMetadata(tok)
	refreshedPlan := accountplan.Normalize(metadata.PlanType)
	account := raw["accounts"].([]any)[0].(map[string]any)
	saved := account["credentials"].(map[string]any)
	oldRefresh, _ := saved["refresh_token"].(string)
	extra, _ := account["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
		account["extra"] = extra
	}
	oldAccountID := refreshedDocumentAccountID(saved, extra)
	oldPlan := accountplan.FromAccount(account)
	workspaceChanged := metadata.ChatGPTAccountID != "" && oldAccountID != "" && metadata.ChatGPTAccountID != oldAccountID
	planChanged := metadata.PlanType != "" && refreshedPlan != "unknown" && oldPlan != "unknown" && refreshedPlan != oldPlan
	if workspaceChanged {
		for _, fields := range []map[string]any{account, saved, extra} {
			for _, field := range []string{"organization_id", "chatgpt_user_id", "plan_type", "chatgpt_plan_type", "planType", "subscription_plan", "subscription_expires_at", "https://api.openai.com/auth.chatgpt_plan_type"} {
				delete(fields, field)
			}
		}
	}
	if workspaceChanged || planChanged || metadata.SubscriptionExpiryReported {
		delete(saved, "subscription_expires_at")
		delete(extra, "subscription_expires_at")
	}
	// Once the provider rotates a refresh token, metadata parsing must never
	// discard it. ID tokens can be omitted or use a provider-specific format.
	saved["access_token"] = tok.AccessToken
	saved["client_id"] = clientID
	extra["client_id"] = clientID
	extra["openai_excel_bps"] = clientID == protocol.DefaultBPSClientID
	protocol.ApplyBPSSettings(extra, clientID)
	protocol.ApplyBPSCredentialSettings(saved, clientID)
	delete(saved, "expires_at")
	delete(saved, "expires_in")
	if metadata.ExpiresAt > 0 {
		saved["expires_at"] = metadata.ExpiresAt
	}
	if tok.RefreshToken != "" {
		saved["refresh_token"] = tok.RefreshToken
	} else {
		saved["refresh_token"] = oldRefresh
	}
	if tok.IDToken != "" {
		saved["id_token"] = tok.IDToken
	}
	if metadata.ChatGPTAccountID != "" {
		saved["chatgpt_account_id"] = metadata.ChatGPTAccountID
		if _, exists := saved["account_id"]; exists {
			saved["account_id"] = metadata.ChatGPTAccountID
		}
		extra["account_id"], extra["chatgpt_account_id"] = metadata.ChatGPTAccountID, metadata.ChatGPTAccountID
	}
	for field, value := range map[string]string{"chatgpt_user_id": metadata.ChatGPTUserID, "organization_id": metadata.OrganizationID, "subscription_expires_at": metadata.SubscriptionExpiresAt, "email": metadata.Email} {
		if value != "" {
			saved[field], extra[field] = value, value
		}
	}
	if refreshedPlan != "unknown" {
		extra["plan_type"] = refreshedPlan
		// Imported documents may carry an explicit tier in more than one
		// location. Keep those existing fields consistent with the new grant.
		for _, fields := range []map[string]any{account, saved, extra} {
			for _, key := range []string{"chatgpt_plan_type", "plan_type", "planType", "subscription_plan", "https://api.openai.com/auth.chatgpt_plan_type"} {
				if _, exists := fields[key]; exists {
					fields[key] = refreshedPlan
				}
			}
		}
	}
	if metadata.PlanType != "" {
		// Sub2API stores the upstream plan enum; the UI uses its normalized name.
		saved["plan_type"] = metadata.PlanType
	}
	raw["type"], raw["version"] = "sub2api-data", 1
	if _, ok := raw["proxies"].([]any); !ok {
		raw["proxies"] = []any{}
	}
	raw["exported_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return raw
}

func refreshedDocumentAccountID(credentials, extra map[string]any) string {
	for _, fields := range []map[string]any{credentials, extra} {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if value, _ := fields[key].(string); strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}
