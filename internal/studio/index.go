//
// Copyright 2026 hyprh.
// SPDX-License-Identifier: Apache-2.0
//

package studio

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder"
	openaiembedder "trpc.group/trpc-go/trpc-agent-go/knowledge/embedder/openai"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/reranker/topk"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore/pgvector"
)

var tableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	select {
	case s.indexSlots <- struct{}{}:
		defer func() { <-s.indexSlots }()
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "an index build is already running")
		return
	}

	var request indexRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateIndexRequest(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, ok := s.getPreview(request.PreviewID)
	if !ok {
		writeError(w, http.StatusNotFound, "preview not found or expired; run Preview again")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	session, err := buildIndex(ctx, request, preview)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.registerIndex(session)

	modes := []string{"vector", "filter"}
	if session.storeType == "pgvector" {
		modes = []string{"hybrid", "vector", "keyword", "filter"}
	}
	writeJSON(w, http.StatusOK, indexResponse{
		ID:            session.id,
		PreviewID:     session.previewID,
		Store:         session.storeType,
		Table:         session.table,
		Documents:     session.count,
		DurationMS:    time.Since(started).Milliseconds(),
		CreatedAt:     session.createdAt,
		SearchModes:   modes,
		FrameworkPath: frameworkPath(),
	})
}

func frameworkPath() string {
	const modulePath = "trpc.group/trpc-go/trpc-agent-go"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return modulePath + "@unknown"
	}
	for _, dependency := range info.Deps {
		if dependency.Path != modulePath {
			continue
		}
		version := dependency.Version
		if dependency.Replace != nil {
			if dependency.Replace.Version != "" {
				version = dependency.Replace.Version
			} else {
				version = "local"
			}
		}
		if version == "" {
			version = "unknown"
		}
		return modulePath + "@" + version
	}
	return modulePath + "@unknown"
}

func validateIndexRequest(request *indexRequest) error {
	if request.PreviewID == "" {
		return fmt.Errorf("preview_id is required")
	}
	if request.Store.Type == "" {
		request.Store.Type = "memory"
	}
	if request.Embedder.Model == "" {
		request.Embedder.Model = openaiembedder.DefaultModel
	}
	if request.Embedder.Dimensions <= 0 {
		request.Embedder.Dimensions = openaiembedder.DefaultDimensions
	}
	if request.Store.Dimensions <= 0 {
		request.Store.Dimensions = request.Embedder.Dimensions
	}
	if request.Store.Dimensions != request.Embedder.Dimensions {
		return fmt.Errorf("vector store dimensions must match embedder dimensions")
	}
	if request.TopK <= 0 {
		request.TopK = 8
	}
	switch request.Store.Type {
	case "memory":
	case "pgvector":
		if strings.TrimSpace(request.Store.DSN) == "" {
			return fmt.Errorf("PGVector DSN is required")
		}
		if request.Store.Table == "" {
			request.Store.Table = "knowledge_studio_documents"
		}
		if !tableNamePattern.MatchString(request.Store.Table) {
			return fmt.Errorf("PGVector table must be a simple SQL identifier")
		}
		switch request.Store.Fusion {
		case "", "rrf":
			request.Store.Fusion = "rrf"
		case "weighted":
		default:
			return fmt.Errorf("unsupported hybrid fusion %q", request.Store.Fusion)
		}
	default:
		return fmt.Errorf("unsupported vector store %q", request.Store.Type)
	}
	return nil
}

func (s *Server) getPreview(id string) (*storedPreview, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	preview, ok := s.previews[id]
	if !ok || time.Now().After(preview.expiresAt) {
		return nil, false
	}
	return &storedPreview{
		response:  preview.response,
		documents: cloneDocuments(preview.documents),
		expiresAt: preview.expiresAt,
	}, true
}

func buildIndex(ctx context.Context, request indexRequest, preview *storedPreview) (*indexSession, error) {
	emb := buildEmbedder(request.Embedder)
	store, err := buildVectorStore(request.Store)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = store.Close()
		}
	}()

	src := &snapshotSource{
		name:      "Knowledge Studio " + request.PreviewID,
		documents: cloneDocuments(preview.documents),
		metadata: map[string]any{
			"studio_preview_id": request.PreviewID,
		},
	}
	kb := knowledge.New(
		knowledge.WithSources([]source.Source{src}),
		knowledge.WithEmbedder(emb),
		knowledge.WithVectorStore(store),
		knowledge.WithReranker(topk.New(topk.WithK(request.TopK))),
		knowledge.WithEnableSourceSync(request.SourceSync),
	)
	if err := kb.Load(
		ctx,
		knowledge.WithRecreate(request.Recreate),
		knowledge.WithShowProgress(false),
		knowledge.WithShowStats(false),
		knowledge.WithDocConcurrency(2),
	); err != nil {
		return nil, fmt.Errorf("load knowledge index: %w", err)
	}
	count, err := store.Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("count indexed documents: %w", err)
	}
	closeOnError = false
	return &indexSession{
		id:        newID("index"),
		previewID: request.PreviewID,
		kb:        kb,
		store:     store,
		storeType: request.Store.Type,
		table:     request.Store.Table,
		count:     count,
		createdAt: time.Now().UTC(),
	}, nil
}

func buildEmbedder(config embedderConfig) embedder.Embedder {
	opts := []openaiembedder.Option{
		openaiembedder.WithModel(config.Model),
		openaiembedder.WithDimensions(config.Dimensions),
	}
	if config.BaseURL != "" {
		opts = append(opts, openaiembedder.WithBaseURL(config.BaseURL))
	}
	if config.APIKey != "" {
		opts = append(opts, openaiembedder.WithAPIKey(config.APIKey))
	}
	return openaiembedder.New(opts...)
}

func buildVectorStore(config storeConfig) (vectorstore.VectorStore, error) {
	if config.Type == "memory" {
		return inmemory.New(), nil
	}
	options := []pgvector.Option{
		pgvector.WithPGVectorClientDSN(config.DSN),
		pgvector.WithTable(config.Table),
		pgvector.WithIndexDimension(config.Dimensions),
		pgvector.WithEnableTSVector(config.EnableTextSearch),
		pgvector.WithVectorIndexType(pgvector.VectorIndexHNSW),
	}
	if config.Fusion == "weighted" {
		options = append(options, pgvector.WithHybridFusionMode(pgvector.HybridFusionWeighted))
	} else {
		options = append(options, pgvector.WithHybridFusionMode(pgvector.HybridFusionRRF))
	}
	store, err := pgvector.New(options...)
	if err != nil {
		return nil, fmt.Errorf("open PGVector: %w", err)
	}
	return store, nil
}

func (s *Server) registerIndex(session *indexSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.indexes) >= maxIndexSessions {
		var oldest *indexSession
		for _, item := range s.indexes {
			if oldest == nil || item.createdAt.Before(oldest.createdAt) {
				oldest = item
			}
		}
		if oldest != nil {
			delete(s.indexes, oldest.id)
			_ = oldest.store.Close()
		}
	}
	s.indexes[session.id] = session
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request searchRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.IndexID == "" {
		writeError(w, http.StatusBadRequest, "index_id is required")
		return
	}
	if strings.TrimSpace(request.Query) == "" {
		writeError(w, http.StatusBadRequest, "query must not be empty")
		return
	}
	if len([]rune(request.Query)) > 16_000 {
		writeError(w, http.StatusBadRequest, "query exceeds 16000 runes")
		return
	}
	if request.MaxResults <= 0 {
		request.MaxResults = 8
	}
	if request.MaxResults > 100 {
		writeError(w, http.StatusBadRequest, "max_results must not exceed 100")
		return
	}
	mode, err := parseSearchMode(request.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.RLock()
	session, ok := s.indexes[request.IndexID]
	s.mu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "index session not found; rebuild the index")
		return
	}
	if session.storeType == "memory" &&
		(mode == vectorstore.SearchModeKeyword || mode == vectorstore.SearchModeHybrid) {
		writeError(w, http.StatusBadRequest, "memory store supports vector and filter modes only")
		return
	}

	var filter *knowledge.SearchFilter
	if len(request.Metadata) > 0 {
		filter = &knowledge.SearchFilter{Metadata: request.Metadata}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	started := time.Now()
	result, err := session.kb.Search(ctx, &knowledge.SearchRequest{
		Query:        request.Query,
		MaxResults:   request.MaxResults,
		MinScore:     request.MinScore,
		SearchMode:   mode,
		SearchFilter: filter,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "search knowledge: "+err.Error())
		return
	}
	results := make([]searchResult, 0, len(result.Documents))
	for i, item := range result.Documents {
		if item == nil || item.Document == nil {
			continue
		}
		results = append(results, searchResult{
			Rank:        i + 1,
			Score:       item.Score,
			DenseScore:  item.Document.Metadata[source.MetadataDenseScore],
			SparseScore: item.Document.Metadata[source.MetadataSparseScore],
			Document:    item.Document,
		})
	}
	if len(results) == 0 && result.Document != nil {
		results = append(results, searchResult{
			Rank:     1,
			Score:    result.Score,
			Document: result.Document,
		})
	}
	writeJSON(w, http.StatusOK, searchResponse{
		Query:      request.Query,
		Mode:       request.Mode,
		DurationMS: time.Since(started).Milliseconds(),
		Results:    results,
	})
}

func parseSearchMode(value string) (vectorstore.SearchMode, error) {
	switch value {
	case "", "hybrid":
		return vectorstore.SearchModeHybrid, nil
	case "vector":
		return vectorstore.SearchModeVector, nil
	case "keyword":
		return vectorstore.SearchModeKeyword, nil
	case "filter":
		return vectorstore.SearchModeFilter, nil
	default:
		return 0, fmt.Errorf("unsupported search mode %q", value)
	}
}

type snapshotSource struct {
	name      string
	documents []*document.Document
	metadata  map[string]any
}

func (s *snapshotSource) ReadDocuments(context.Context) ([]*document.Document, error) {
	return cloneDocuments(s.documents), nil
}

func (s *snapshotSource) Name() string {
	return s.name
}

func (s *snapshotSource) Type() string {
	return source.TypeFile
}

func (s *snapshotSource) GetMetadata() map[string]any {
	return cloneMap(s.metadata)
}
