//
// Copyright 2026 hyprh.
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyprh/trpc-agent-go-knowledge-studio/internal/studio"
)

//go:embed web samples
var assets embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "HTTP listen address")
	flag.Parse()

	webFS, err := fs.Sub(assets, "web")
	if err != nil {
		log.Fatalf("prepare web assets: %v", err)
	}
	sampleFS, err := fs.Sub(assets, "samples")
	if err != nil {
		log.Fatalf("prepare samples: %v", err)
	}

	app := studio.New(webFS, sampleFS)
	server := &http.Server{
		Addr:              *addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("Knowledge Studio is running at http://%s", *addr)
		errCh <- server.ListenAndServe()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-signals:
		log.Printf("received %s, shutting down", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := app.Close(); err != nil {
		log.Printf("close studio: %v", err)
	}
}
