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
	return runProxy(cfg)
}

// runProxy serves until a signal or a listener failure, and returns the exit code.
func runProxy(cfg config) int {
	application := newProxy(cfg)
	defer application.client.CloseIdleConnections()
	if cfg.usesChatGPT() {
		session, err := openOAuthSession(cfg, application.client)
		if err != nil {
			slog.Error("could not load ChatGPT session", "reason", err.Error())
			return 1
		}
		application.oauth = session
		defer func() {
			cleanupContext, cancelCleanup := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancelCleanup()
			if err := application.oauth.close(cleanupContext); err != nil {
				slog.Warn("could not remove credential lock", "reason", err.Error())
			}
		}()
		slog.Info("using saved ChatGPT account", "account", cfg.account)
	}

	listeners, err := openListeners(cfg.listenAddresses)
	if err != nil {
		slog.Error("could not open listener", "reason", err.Error())
		return 1
	}

	// Keep requests alive during the graceful shutdown period.
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

	stopContext, stopSignals := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stopSignals()

	// All listeners share one server, so the proxy key and limits cover every address.
	// The channel holds one result per listener, so no Serve goroutine blocks on send.
	serveErrors := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func() { serveErrors <- httpServer.Serve(listener) }()
		slog.Info("proxy listening", "address", listener.Addr().String())
	}
	if cfg.proxyAPIKey == "" {
		slog.Warn("local authentication is disabled; set ANT2OAI_API_KEY or JSON proxy_api_key")
	}

	exitCode := 0
	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		// Stop the whole proxy, because a partly reachable proxy is hard to notice.
		// The graceful shutdown below lets requests on the other listeners finish.
		slog.Error("HTTP server failed", "reason", err.Error())
		exitCode = 1
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
	return exitCode
}

// shutdownSignals lists the signals that start a graceful shutdown. A terminal
// sends SIGHUP when it closes. SIGHUP is left out when the process started with
// it ignored, for example under nohup, because Notify would stop ignoring it.
func shutdownSignals() []os.Signal {
	signals := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		signals = append(signals, syscall.SIGHUP)
	}
	return signals
}

// openListeners opens every address or none, so a failed start keeps no port bound.
func openListeners(addresses []string) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addresses))
	for _, address := range addresses {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			for _, opened := range listeners {
				err = errors.Join(err, opened.Close())
			}
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}
