package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cli-to-api/internal/api"
	"cli-to-api/internal/auth"
	"cli-to-api/internal/secret"
	"cli-to-api/internal/session"
)

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address (default: the stored one, else :8080)")
	timeout := fs.Duration("timeout", 5*time.Minute, "default per-request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	box, err := secret.Open(st.Dir())
	if err != nil {
		return fmt.Errorf("opening the secret box: %w", err)
	}

	sessions, err := session.Load(st.Dir())
	if err != nil {
		return fmt.Errorf("opening the conversation store: %w", err)
	}

	listen := *addr
	if listen == "" {
		listen = st.Addr()
	}
	if listen == "" {
		listen = ":8080"
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	handler := api.New(api.Options{
		Auth:           authBridge{v: auth.NewVerifier(st)},
		Usage:          st,
		Logger:         logger,
		Version:        version,
		Timeout:        *timeout,
		ResolveOptions: optionResolver(box),
		Conversations:  convBridge{s: sessions},
	})

	keys := st.List()
	active := 0
	for _, k := range keys {
		if !k.Revoked {
			active++
		}
	}
	logger.Printf("cli-to-api %s listening on %s", version, listen)
	logger.Printf("config %s | %d active key(s)", st.Path(), active)
	for _, k := range keys {
		if k.Revoked {
			continue
		}
		logger.Printf("  %s  %s  adapter=%s models=%v", shortID(k.ID), k.Name, k.Profile.Adapter, k.Profile.Models)
	}
	if active == 0 {
		logger.Printf("no usable keys yet — run: cli-to-api key create")
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a streaming response outlives any fixed deadline,
		// and the per-request context already bounds the work.
	}

	// Shut down on signal so in-flight CLI subprocesses get their contexts
	// cancelled rather than being orphaned.
	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case s := <-sig:
		logger.Printf("received %s, shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
		// Flush any usage counters buffered to avoid a disk write per request.
		if err := st.Save(); err != nil {
			logger.Printf("saving usage counters: %v", err)
		}
		// Conversation timestamps are buffered too; flush before exiting so a
		// restart does not expire live conversations early.
		if err := sessions.Save(); err != nil {
			logger.Printf("saving conversation state: %v", err)
		}
		return nil
	}
}
