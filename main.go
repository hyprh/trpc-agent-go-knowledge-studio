// Command workbench serves the Knowledge Workbench control plane and UI.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/api"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/engine"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/readers"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/store"
)

//go:embed all:web
var webFS embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "listen address; loopback by default")
	dbPath := flag.String("db", "workbench.db", "path to the control-plane SQLite database")
	samples := flag.String("samples", "",
		"directory to seed a demo knowledge base from, when the database is empty")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	hub := engine.NewHub()
	eng := engine.New(st, hub)
	defer eng.Close()

	if err := eng.Resume(context.Background()); err != nil {
		log.Printf("resume pending tasks: %v", err)
	}
	if *samples != "" {
		if err := seedSamples(context.Background(), st, *samples); err != nil {
			log.Printf("seed samples: %v", err)
		}
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("web assets: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, eng, hub, sub).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /api/events is a long-lived stream.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("knowledge workbench listening on http://%s", *addr)
		log.Printf("control plane: %s", *dbPath)
		log.Printf("readers registered for: %v", readers.Extensions())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// seedSamples creates a demo knowledge base over a directory of mixed-format
// files, so a fresh database has something to inspect immediately.
//
// It uses one dir source with no reader override: each file then resolves its
// own reader by extension, which is what makes the sample set worth shipping —
// .md chunks as Markdown, .json in serialized bytes, .csv per record. It is
// deliberately not an auto source: auto delegates without passing its name
// down, so its documents cannot be attributed back and cannot be deleted.
func seedSamples(ctx context.Context, st *store.Store, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("samples path %q is not a directory", dir)
	}
	bases, err := st.ListBases(ctx)
	if err != nil {
		return err
	}
	if len(bases) > 0 {
		return nil // only seed an empty control plane
	}

	base := &model.Base{
		Name:             "Chunking samples",
		Description:      "trpc-agent-go 的分块样例文件：markdown、txt、csv、json 各一套，用来对比不同 reader 与策略。",
		VectorStore:      model.VectorStoreConfig{Type: "inmemory"},
		EnableSourceSync: true,
		// Upstream defaults: 1024 with no overlap, and no strategy override so
		// each reader picks its own.
		SourceDefaults: model.SourceDefaults{ChunkSize: 1024, ChunkOverlap: 0},
		Retrieval:      model.RetrievalConfig{TopK: 5},
	}
	if err := st.CreateBase(ctx, base); err != nil {
		return err
	}
	recursive := true
	src := &model.Source{
		BaseID:      base.ID,
		DisplayName: "samples/",
		Type:        model.SourceDir,
		Config: model.SourceConfig{
			Dirs:         []string{abs},
			Recursive:    &recursive,
			ChunkSize:    1024,
			ChunkOverlap: 0,
			// No Strategy and no ReaderType: every file picks its own.
		},
		State: model.SourceActive,
	}
	if err := st.CreateSource(ctx, src); err != nil {
		return err
	}
	log.Printf("seeded demo base %q from %s", base.Name, abs)
	return nil
}
