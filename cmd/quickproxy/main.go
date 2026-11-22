package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/admin"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/oauth"
	"quickwork-openai/internal/proxy"
	"quickwork-openai/internal/requestlog"
	"quickwork-openai/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		return serve(nil)
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "login":
		return login(args[1:])
	case "accounts", "list":
		return listAccounts(args[1:])
	case "refresh":
		return refreshAccounts(args[1:])
	case "logout":
		return logout(args[1:])
	case "admin-hash":
		return adminHash(args[1:])
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		if strings.HasPrefix(args[0], "-") {
			return serve(args)
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quickwork-openai - Amazon Quick to OpenAI-compatible API

Usage:
  quickproxy serve    [-config config.json]
  quickproxy login    [-config config.json] [-provider social|enterprise|gateway|social_gateway]
                      [-stage prod|beta|integ|dev] [-region REGION] [-port 18080]
                      [-timeout 1h] [-open] [-manual]
  quickproxy accounts [-config config.json]
  quickproxy refresh  [-config config.json] [-account ID]
  quickproxy logout   [-config config.json] (-all | -account ID)
  quickproxy admin-hash [-generate]  # otherwise read password from stdin

Running without a subcommand is equivalent to "serve".

config.json is created with defaults on first use. Accounts are stored in the
SQLite database named by "database" in config.json. When multiple accounts are
present, requests rotate through them round-robin.
`)
}

func adminHash(args []string) int {
	fs := flag.NewFlagSet("admin-hash", flag.ExitOnError)
	generate := fs.Bool("generate", false, "generate a password and print credentials as JSON")
	_ = fs.Parse(args)
	var password string
	if *generate {
		secret := make([]byte, 18)
		if _, err := rand.Read(secret); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		password = base64.RawURLEncoding.EncodeToString(secret)
	} else {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1026))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	}
	hash, err := admin.HashPassword(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *generate {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"password": password, "password_hash": hash})
	} else {
		fmt.Println(hash)
	}
	return 0
}

func loadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

func openStore(cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", cfg.Database, err)
	}
	return st, nil
}

func serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	diagnosticLog, err := diagnostics.Open(diagnostics.Options{Level: cfg.Logging.Level, File: cfg.Logging.File, MaxMB: cfg.Logging.MaxMB, Backups: cfg.Logging.Backups, Stdout: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "logging:", err)
		return 1
	}
	diagnostics.SetDefault(diagnosticLog)
	defer func() { diagnostics.SetDefault(nil); _ = diagnosticLog.Close() }()
	diagnostics.RegisterSecrets(append(cfg.APIKeys, cfg.Admin.PasswordHash)...)
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	requestLogs, err := requestlog.New(st, requestlog.DefaultOptions(cfg.RequestLogDirectory))
	if err != nil {
		fmt.Fprintln(os.Stderr, "request logs:", err)
		return 1
	}
	defer requestLogs.Close()

	mgr := accounts.New(st, cfg)
	count, _ := mgr.EnabledCount(context.Background())
	apiServer := proxy.NewServer(cfg, mgr, st)
	if err := apiServer.InitMedia(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "media:", err)
		return 1
	}
	handler := apiServer.Handler()
	if cfg.Admin.Enabled {
		console, err := admin.New(cfg, st, mgr, apiServer)
		if err != nil {
			fmt.Fprintln(os.Stderr, "admin:", err)
			return 1
		}
		console.SetRequestLogs(requestLogs)
		mux := http.NewServeMux()
		mux.Handle("/admin/", console.Handler())
		mux.Handle("/", handler)
		handler = mux
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           diagnostics.Middleware(requestLogs.Middleware(handler)),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	diagnostics.Event(context.Background(), "info", "server.started", "addr", cfg.Addr, "accounts", count, "auth_enabled", len(cfg.APIKeys) > 0, "logging_level", cfg.Logging.Level, "logging_file", cfg.Logging.File, "request_timeout", cfg.RequestTimeout.String())
	if count == 0 {
		diagnostics.Event(context.Background(), "warn", "server.no_accounts")
	}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	listenErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			cancel()
		}
	}()

	<-stop.Done()
	diagnostics.Event(context.Background(), "info", "server.stopping")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	select {
	case err := <-listenErr:
		diagnostics.Event(context.Background(), "error", "server.listen_failed", "error", err)
		return 1
	default:
		return 0
	}
}

func login(args []string) int {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	provider := fs.String("provider", "", "social, enterprise, gateway or social_gateway (default from config)")
	stage := fs.String("stage", "", "prod, beta, integ or dev (default from config)")
	region := fs.String("region", "", "IDC region (social) or enterprise discovery region prefix")
	port := fs.Int("port", 0, "localhost callback port (default from config)")
	timeout := fs.Duration("timeout", 0, "wait duration for the browser callback (default from config)")
	open := fs.Bool("open", false, "open the login URL in the default browser")
	manual := fs.Bool("manual", false, "paste browser callback URLs manually (for servers without a reachable localhost)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	if *provider == "" {
		*provider = cfg.OAuth.Provider
	}
	if *stage == "" {
		*stage = cfg.OAuth.BuildStage
	}
	if *port == 0 {
		*port = cfg.OAuth.CallbackPort
	}
	if *timeout == 0 {
		*timeout = cfg.OAuth.Timeout.Duration()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := log.New(os.Stdout, "[login] ", 0)
	opts := oauth.LoginOptions{
		Provider:    *provider,
		Stage:       *stage,
		Region:      *region,
		Port:        *port,
		Timeout:     *timeout,
		AppVersion:  cfg.AppVersion,
		OpenBrowser: *open,
		Logf:        logger.Printf,
	}
	var acct *store.Account
	if *manual {
		acct, err = oauth.LoginManual(ctx, opts, st, os.Stdin, os.Stdout)
	} else {
		acct, err = oauth.Login(ctx, opts, st)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[login] failed:", err)
		return 1
	}
	fmt.Printf("account %d ready: %s (%s, %s)\n", acct.ID, displayAccount(acct), acct.Provider, acct.WSHost)
	return 0
}

func listAccounts(args []string) int {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	list, err := st.List(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "list accounts:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tPROVIDER\tSTAGE\tREGION\tEXPIRES\tUSES\tSTATUS")
	now := time.Now()
	for i := range list {
		a := &list[i]
		status := "ready"
		if a.Disabled {
			status = "disabled"
		} else if a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix() {
			status = "expired"
		}
		if a.LastError != "" {
			status += " (" + a.LastError + ")"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			a.ID, displayAccount(a), a.Provider, a.BuildStage, a.Region,
			formatEpoch(a.ExpiresAt), a.UseCount, status)
	}
	_ = tw.Flush()
	return 0
}

func refreshAccounts(args []string) int {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "refresh only this account ID (default: all)")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	mgr := accounts.New(st, cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *accountID > 0 {
		a, err := mgr.RefreshOne(ctx, *accountID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "account %d: %v\n", *accountID, err)
			return 1
		}
		fmt.Printf("account %d refreshed: %s expires %s\n", a.ID, displayAccount(a), formatEpoch(a.ExpiresAt))
		return 0
	}

	results, err := mgr.RefreshAll(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refresh:", err)
		return 1
	}
	if len(results) == 0 {
		fmt.Println("no accounts; run `quickproxy login`")
		return 0
	}
	failed := 0
	for _, res := range results {
		if res.Err != nil {
			failed++
			id := int64(0)
			if res.Account != nil {
				id = res.Account.ID
			}
			fmt.Fprintf(os.Stderr, "account %d: %v\n", id, res.Err)
			continue
		}
		fmt.Printf("account %d refreshed: %s expires %s\n",
			res.Account.ID, displayAccount(res.Account), formatEpoch(res.Account.ExpiresAt))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func logout(args []string) int {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	configPath := fs.String("config", "config.json", "path to config.json")
	accountID := fs.Int64("account", 0, "delete only this account ID")
	all := fs.Bool("all", false, "delete every stored account")
	_ = fs.Parse(args)

	if *accountID <= 0 && !*all {
		fmt.Fprintln(os.Stderr, "logout requires -all or -account ID")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()

	ctx := context.Background()
	if *all {
		n, err := st.DeleteAll(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "logout:", err)
			return 1
		}
		fmt.Printf("removed %d account(s)\n", n)
		return 0
	}
	ok, err := st.Delete(ctx, *accountID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "account %d not found\n", *accountID)
		return 1
	}
	fmt.Printf("removed account %d\n", *accountID)
	return 0
}

func displayAccount(a *store.Account) string {
	for _, v := range []string{a.Email, a.AccountName, a.AWSAccountID} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "-"
}

func formatEpoch(v int64) string {
	if v <= 0 {
		return "unknown"
	}
	return time.Unix(v, 0).Local().Format("2006-01-02 15:04")
}
