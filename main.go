package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/RP2/gh-vault/internal/backup"
	"github.com/RP2/gh-vault/internal/config"
	"github.com/RP2/gh-vault/internal/github"
	"github.com/RP2/gh-vault/internal/scheduler"
	"github.com/RP2/gh-vault/internal/store"
	reposync "github.com/RP2/gh-vault/internal/sync"
	"github.com/RP2/gh-vault/internal/tlsutil"
	"github.com/RP2/gh-vault/internal/web"
)

func main() {
	cfg := config.Load()

	certPath := ""
	keyPath := ""
	if !cfg.DisableTLS {
		var err error
		certPath, keyPath, err = tlsutil.EnsureSelfSignedCert(cfg.DataDir)
		if err != nil {
			slog.Error("failed to ensure TLS certificate", "error", err)
			os.Exit(1)
		}
	}

	db, err := store.New(cfg.DataDir + "/gh-vault.db")
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	tokenProvider := github.NewDBTokenProvider(db.Secrets(), cfg.EncryptionKey)
	ghClient := github.NewClient(tokenProvider)
	backupEngine := backup.NewEngine(cfg.BackupDir, tokenProvider, db.Repos())

	// Reset stored backup state for repos whose files are missing from disk
	// (dataset restore, manual deletion). The next backup run re-clones them.
	if err := backupEngine.Reconcile(context.Background()); err != nil {
		slog.Warn("startup: backup state reconcile failed", "error", err)
	}

	syncer := reposync.NewSyncer(ghClient, db.Repos(), db.Logs(), backupEngine)
	sessions := db.Sessions()
	sched := scheduler.New(db.Settings(), syncer, backupEngine, db.Repos(), db.Logs(), sessions)
	srv, err := web.NewServer(cfg, db.Users(), db.Settings(), db.Repos(), db.Logs(), db.Secrets(), sessions, backupEngine, syncer, sched, tokenProvider, ghClient)
	if err != nil {
		slog.Error("failed to create web server", "error", err)
		db.Close()
		os.Exit(1)
	}
	defer srv.Stop()

	if err := sched.Start(); err != nil {
		slog.Error("failed to start scheduler", "error", err)
		db.Close()
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           srv,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig: &tls.Config{
			MinVersion:       tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down...")
		sched.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
		tokenProvider.Wipe()
	}()

	if cfg.DisableTLS {
		slog.Info("starting server", "port", cfg.Port, "tls", false)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			sched.Stop()
			db.Close()
			os.Exit(1)
		}
	} else {
		slog.Info("starting server", "port", cfg.Port, "tls", true, "cert", certPath)
		if err := httpServer.ListenAndServeTLS(certPath, keyPath); err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			sched.Stop()
			db.Close()
			os.Exit(1)
		}
	}
	<-done
}
