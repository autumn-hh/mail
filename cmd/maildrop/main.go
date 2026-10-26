package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"maildrop/internal/maildrop"
)

func run() error {
	importFile := flag.String("import-json", "", "Import a legacy JSON inbox into SQLite and exit (source stays unchanged)")
	healthcheck := flag.Bool("healthcheck", false, "Probe local HTTP health and exit")
	flag.Parse()
	c, e := maildrop.LoadConfig()
	if e != nil {
		return e
	}
	if *healthcheck {
		_, port, e := net.SplitHostPort(c.HTTPAddr)
		if e != nil {
			return e
		}
		client := http.Client{Timeout: 5 * time.Second}
		r, e := client.Get("http://127.0.0.1:" + port + "/healthz")
		if e != nil {
			return e
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("health check: HTTP %d", r.StatusCode)
		}
		return nil
	}
	store, e := maildrop.OpenStore(c)
	if e != nil {
		return e
	}
	defer store.DB.Close()
	if *importFile != "" {
		count, e := store.ImportJSON(*importFile)
		if e == nil {
			slog.Info("Legacy import complete", "mailboxes", count)
		}
		return e
	}
	app, e := maildrop.NewApp(store)
	if e != nil {
		return e
	}
	webListener, e := net.Listen("tcp", c.HTTPAddr)
	if e != nil {
		return e
	}
	defer webListener.Close()
	smtpListener, e := net.Listen("tcp", c.SMTPAddr)
	if e != nil {
		return e
	}
	defer smtpListener.Close()
	web := &http.Server{Handler: app, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	smtp := maildrop.NewSMTP(store)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 2)
	go func() { errs <- web.Serve(webListener) }()
	go func() { errs <- smtp.Serve(smtpListener) }()
	slog.Info("Maildrop Go started", "http", c.HTTPAddr, "smtp", c.SMTPAddr)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var result error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case e := <-errs:
			if !errors.Is(e, http.ErrServerClosed) {
				result = e
			}
			break loop
		case now := <-ticker.C:
			if e := store.Cleanup(now); e != nil {
				slog.Error("Cleanup failed", "error", e)
			}
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e = web.Shutdown(shutdown); e != nil {
		slog.Warn("HTTP shutdown", "error", e)
	}
	if e = smtp.Shutdown(shutdown); e != nil {
		slog.Warn("SMTP shutdown", "error", e)
		smtp.Close()
	}
	return result
}
func main() {
	if e := run(); e != nil {
		slog.Error("Maildrop stopped", "error", e)
		os.Exit(1)
	}
}
