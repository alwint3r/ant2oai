package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(arguments []string) int {
	command := "serve"
	if len(arguments) > 0 {
		switch arguments[0] {
		case "serve", "login", "login-status", "logout", "models":
			command, arguments = arguments[0], arguments[1:]
		}
	}
	cfg, err := loadConfig(arguments, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		slog.Error("invalid configuration", "reason", err.Error())
		return 2
	}

	if cfg.requestConsent && command != "login" {
		slog.Error("-reconsent is available only with the login command")
		return 2
	}
	if command != "serve" {
		if err := runAuthCommand(command, cfg, os.Stdout); err != nil {
			slog.Error("ChatGPT command failed", "reason", err.Error())
			return 1
		}
		return 0
	}

	application := newProxy(cfg)
	defer application.client.CloseIdleConnections()
	if cfg.authMode == "chatgpt" {
		application.oauth, err = openOAuthSession(cfg, application.client)
		if err != nil {
			slog.Error("could not load ChatGPT session", "reason", err.Error())
			return 1
		}
		defer func() {
			cleanupContext, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelCleanup()
			if err := application.oauth.close(cleanupContext); err != nil {
				slog.Warn("could not remove credential lock", "reason", err.Error())
			}
		}()
		slog.Info("using saved ChatGPT account", "account", cfg.account)
	}

	listener, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		slog.Error("could not open listener", "reason", err.Error())
		return 1
	}

	// Keep requests alive during the graceful shutdown period after a signal.
	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	httpServer := &http.Server{
		Handler:           application,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Streaming writes have a separate deadline for each event.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 32 << 10,
		BaseContext:    func(net.Listener) context.Context { return requestContext },
	}

	stopContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- httpServer.Serve(listener) }()

	slog.Info("proxy listening", "address", listener.Addr().String())
	if cfg.proxyAPIKey == "" {
		slog.Warn("local authentication is disabled; set ANT2OAI_API_KEY or JSON proxy_api_key")
	}

	select {
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed")
			return 1
		}
		return 0
	case <-stopContext.Done():
	}

	// Restore default signal handling so another interrupt can force an exit.
	stopSignals()
	slog.Info("shutting down; allowing active requests up to 15 seconds")
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		slog.Warn("graceful shutdown deadline reached; cancelling active requests")
		cancelRequests()
		if err := httpServer.Close(); err != nil {
			slog.Error("could not close HTTP server")
			return 1
		}
	}
	return 0
}
