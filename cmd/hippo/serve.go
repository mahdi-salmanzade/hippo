package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/mahdi-salmanzade/hippo/web"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", web.DefaultConfigPath, "path to config file")
	addr := fs.String("addr", "", "host:port to bind (overrides config.server.addr)")
	bind := fs.String("bind", "", "shorthand for setting just the bind host (keeps port from config)")
	authToken := fs.String("auth-token", "", "auth token required for non-localhost binds (overrides config)")
	openBrowser := fs.Bool("open", false, "open the UI in the default browser")
	logLevel := fs.String("log-level", "info", "log level: debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	web.Version = version

	cfg, err := web.Load(*configPath)
	if err != nil {
		if !os.IsNotExist(unwrapPathErr(err)) {
			return err
		}
		// First-run: create the default config and continue.
		cfg, err = web.InitConfig(*configPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "hippo: created %s\n", cfg.Path())
	}

	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	if *bind != "" {
		// Keep whatever port the config held; replace the host.
		port := "7844"
		if i := strings.LastIndex(cfg.Server.Addr, ":"); i >= 0 {
			port = cfg.Server.Addr[i+1:]
		}
		cfg.Server.Addr = *bind + ":" + port
	}
	if *authToken != "" {
		cfg.Server.AuthToken = *authToken
	}

	logger := newLogger(*logLevel)

	srv, err := web.New(cfg, web.WithLogger(logger))
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *openBrowser {
		go openURL("http://" + cfg.Server.Addr)
	}

	fmt.Fprintf(os.Stderr, "hippo: serving on http://%s (Ctrl-C to stop)\n", cfg.Server.Addr)
	return srv.Start(ctx)
}

// unwrapPathErr digs through wrapping added by web.Load so os.IsNotExist
// still recognises the missing-file case.
func unwrapPathErr(err error) error {
	for err != nil {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		err = inner
	}
	return err
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// openURL launches the platform's default browser at url. Best-effort;
// failures are silent because `--open` is a convenience flag.
func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	_ = cmd.Start()
}
