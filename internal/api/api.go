// Package api exposes the control plane over HTTP.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/searchfilter"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/engine"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/preview"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/store"
)

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 4 << 20

// Server serves the API and the static UI.
type Server struct {
	st  *store.Store
	eng *engine.Engine
	hub *engine.Hub
	web fs.FS
}

// New creates an API server.
func New(st *store.Store, eng *engine.Engine, hub *engine.Hub, web fs.FS) *Server {
	return &Server{st: st, eng: eng, hub: hub, web: web}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/bases", s.listBases)
	mux.HandleFunc("POST /api/bases", s.createBase)
	mux.HandleFunc("GET /api/bases/{id}", s.getBase)
	mux.HandleFunc("PATCH /api/bases/{id}", s.patchBase)
	mux.HandleFunc("DELETE /api/bases/{id}", s.deleteBase)

	mux.HandleFunc("GET /api/bases/{id}/sources", s.listSources)
	mux.HandleFunc("POST /api/bases/{id}/sources", s.createSource)
	mux.HandleFunc("POST /api/bases/{id}/sync", s.syncAll)
	mux.HandleFunc("PATCH /api/sources/{sid}", s.patchSource)
	mux.HandleFunc("DELETE /api/sources/{sid}", s.deleteSource)
	mux.HandleFunc("POST /api/sources/{sid}/sync", s.syncSource)
	mux.HandleFunc("GET /api/sources/{sid}/tree", s.sourceTree)

	mux.HandleFunc("POST /api/preview", s.previewChunks)
	mux.HandleFunc("POST /api/preview/file", s.previewFile)
	mux.HandleFunc("POST /api/bases/{id}/search", s.search)
	mux.HandleFunc("GET /api/bases/{id}/capabilities", s.capabilities)

	mux.HandleFunc("GET /api/bases/{id}/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks/{tid}/cancel", s.cancelTask)
	mux.HandleFunc("GET /api/events", s.events)

	assets, err := newAssetHandler(s.web)
	if err != nil {
		// The UI is embedded at build time, so this cannot fail at runtime for
		// any reason the operator could fix.
		panic(err)
	}
	mux.Handle("/", assets)
	return mux
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// fail maps domain errors onto status codes so the UI can show the real reason
// instead of a generic 500.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, model.ErrValidation), errors.Is(err, engine.ErrEmbedderNotConfigured):
		// A rejected configuration is the caller's to fix, not a server fault.
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	default:
		var unsupported engine.ErrStoreUnsupported
		if errors.As(err, &unsupported) {
			writeErr(w, http.StatusNotImplemented, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------- bases

func (s *Server) listBases(w http.ResponseWriter, r *http.Request) {
	bases, err := s.st.ListBases(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if bases == nil {
		bases = []*model.Base{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bases": bases})
}

func (s *Server) createBase(w http.ResponseWriter, r *http.Request) {
	var b model.Base
	if !decode(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	b.Description = strings.TrimSpace(b.Description)
	if err := validateBaseText(b.Name, b.Description); err != nil {
		fail(w, err)
		return
	}
	if b.VectorStore.Type == "" {
		b.VectorStore.Type = "inmemory"
	}
	if b.SourceDefaults.ChunkSize == 0 {
		// Mirror upstream: chunking.defaultChunkSize is 1024 and
		// chunking.defaultOverlap is 0. Overlap is opt-in there, so a base
		// created here behaves like one wired up in Go with no options.
		b.SourceDefaults = model.SourceDefaults{ChunkSize: 1024, ChunkOverlap: 0}
	}
	if b.Retrieval.TopK == 0 {
		b.Retrieval.TopK = 5
	}
	b.ID = ""
	if err := s.st.CreateBase(r.Context(), &b); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, &b)
}

// Bounds on free text. Nothing enforced them before, so a name long enough to
// break every list and dropdown in the UI was accepted and then had to be
// found and deleted through the API.
const (
	maxNameLen = 120
	maxDescLen = 2000
)

func validateBaseText(name, desc string) error {
	if name == "" {
		return fmt.Errorf("%w: 名称不能为空", model.ErrValidation)
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return fmt.Errorf("%w: 名称最长 %d 个字符", model.ErrValidation, maxNameLen)
	}
	if utf8.RuneCountInString(desc) > maxDescLen {
		return fmt.Errorf("%w: 描述最长 %d 个字符", model.ErrValidation, maxDescLen)
	}
	return nil
}

func (s *Server) getBase(w http.ResponseWriter, r *http.Request) {
	b, err := s.st.GetBase(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) patchBase(w http.ResponseWriter, r *http.Request) {
	cur, err := s.st.GetBase(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	var in model.Base
	if !decode(w, r, &in) {
		return
	}
	// An absent key means "keep the stored one". The API never returns it, so a
	// client that round-trips a base it fetched has no key to send back and
	// must not be taken to mean "clear it".
	if in.Embedder.APIKey == "" {
		in.Embedder.APIKey = cur.Embedder.APIKey
	}

	// Changing the embedding model or dimension invalidates every vector
	// already written, so bump the revision and let the UI surface it rather
	// than silently re-embedding 100k rows.
	reindex := in.Embedder.Model != cur.Embedder.Model ||
		in.Embedder.Dimensions != cur.Embedder.Dimensions ||
		in.VectorStore.Type != cur.VectorStore.Type

	// Anything the engine fingerprints forces the instance to be rebuilt.
	// Renames, descriptions and retrieval defaults do not — dropping the
	// instance for those discards an in-memory index that nothing will rebuild,
	// leaving sources reporting "synced" over an empty store.
	rebuild := reindex ||
		in.Embedder.Provider != cur.Embedder.Provider ||
		in.Embedder.BaseURL != cur.Embedder.BaseURL ||
		in.Embedder.APIKey != cur.Embedder.APIKey ||
		in.EnableSourceSync != cur.EnableSourceSync

	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	if err := validateBaseText(in.Name, in.Description); err != nil {
		fail(w, err)
		return
	}
	cur.Name = in.Name
	cur.Description = in.Description
	cur.VectorStore = in.VectorStore
	cur.Embedder = in.Embedder
	cur.EnableSourceSync = in.EnableSourceSync
	cur.SourceDefaults = in.SourceDefaults
	cur.Retrieval = in.Retrieval
	if reindex {
		cur.IndexRevision++
	}
	if err := s.st.UpdateBase(r.Context(), cur); err != nil {
		fail(w, err)
		return
	}
	reset := int64(0)
	if rebuild {
		// Dropping the instance discards a non-persistent index, so the ingest
		// state has to go with it or the UI keeps claiming those chunks exist.
		if !cur.VectorStore.Persistent() {
			reset, _ = s.st.ResetSyncState(r.Context(), cur.ID)
		}
		s.eng.Invalidate(cur.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"base":             cur,
		"reindex_required": reindex,
		"index_dropped":    reset,
	})
}

func (s *Server) deleteBase(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.DeleteBase(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	s.eng.Invalidate(id)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- sources

type sourceInput struct {
	DisplayName  string             `json:"display_name"`
	Type         model.SourceType   `json:"type"`
	Config       model.SourceConfig `json:"config"`
	UserMetadata map[string]any     `json:"user_metadata,omitempty"`
	State        model.SourceState  `json:"state,omitempty"`
	Sync         bool               `json:"sync,omitempty"`
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := s.st.ListSources(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, sourceView(src))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// sourceView adds the derived fields the UI needs. They are computed rather
// than stored so they can never disagree with the config hash.
func sourceView(src *model.Source) map[string]any {
	return map[string]any{
		"source":            src,
		"sync_status":       src.SyncStatus(),
		"needs_sync":        src.NeedsSync(),
		"supports_chunking": model.SupportsChunking(src.Type),
	}
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	baseID := r.PathValue("id")
	if _, err := s.st.GetBase(r.Context(), baseID); err != nil {
		fail(w, err)
		return
	}
	var in sourceInput
	if !decode(w, r, &in) {
		return
	}
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if err := validateBaseText(in.DisplayName, ""); err != nil {
		fail(w, err)
		return
	}
	if err := in.Config.Validate(in.Type); err != nil {
		fail(w, err)
		return
	}
	if err := model.ValidateMetadata(in.UserMetadata); err != nil {
		fail(w, err)
		return
	}
	src := &model.Source{
		BaseID:       baseID,
		DisplayName:  in.DisplayName,
		Type:         in.Type,
		Config:       in.Config,
		UserMetadata: in.UserMetadata,
		State:        model.SourceActive,
	}
	if err := s.st.CreateSource(r.Context(), src); err != nil {
		fail(w, err)
		return
	}
	resp := map[string]any{"source": src}
	if in.Sync {
		t := snapshotTask(baseID, src, model.TaskAddSource)
		if err := s.eng.Enqueue(r.Context(), t); err != nil {
			fail(w, err)
			return
		}
		resp["task"] = t
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) patchSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.st.GetSource(r.Context(), r.PathValue("sid"))
	if err != nil {
		fail(w, err)
		return
	}
	var in sourceInput
	if !decode(w, r, &in) {
		return
	}
	if in.Type == "" {
		in.Type = src.Type
	}
	if err := in.Config.Validate(in.Type); err != nil {
		fail(w, err)
		return
	}
	if err := model.ValidateMetadata(in.UserMetadata); err != nil {
		fail(w, err)
		return
	}
	if in.DisplayName != "" {
		src.DisplayName = in.DisplayName
	}
	src.Type = in.Type
	src.Config = in.Config
	src.UserMetadata = in.UserMetadata
	if in.State != "" {
		src.State = in.State
	}
	// The edit lands in the database only. Upstream keeps serving the previous
	// configuration until a sync task runs, which is what makes the derived
	// "stale" state meaningful.
	if err := s.st.UpdateSourceConfig(r.Context(), src); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sourceView(src))
}

func (s *Server) syncSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.st.GetSource(r.Context(), r.PathValue("sid"))
	if err != nil {
		fail(w, err)
		return
	}
	kind := model.TaskReloadSource
	if src.NeverSynced() {
		kind = model.TaskAddSource
	}
	t := snapshotTask(src.BaseID, src, kind)
	if err := s.eng.Enqueue(r.Context(), t); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// snapshotTask freezes the source as it stands now. The engine executes and
// records against this copy, so an edit made while the task is queued or
// running belongs to the next sync rather than being credited to this one.
func snapshotTask(baseID string, src *model.Source, kind model.TaskKind) *model.Task {
	return &model.Task{
		BaseID:     baseID,
		SourceID:   src.ID,
		Kind:       kind,
		Params:     src.Config,
		ConfigHash: src.ConfigHash,
		Metadata:   src.UserMetadata,
	}
}

func (s *Server) syncAll(w http.ResponseWriter, r *http.Request) {
	baseID := r.PathValue("id")
	srcs, err := s.st.ListSources(r.Context(), baseID)
	if err != nil {
		fail(w, err)
		return
	}
	// Enqueue one task per source rather than a single Load: writes serialize
	// on a per-base mutex upstream anyway, and per-source tasks give the UI a
	// row-level progress bar and a cancel button.
	var tasks []*model.Task
	for _, src := range srcs {
		if src.State == model.SourceDisabled {
			continue
		}
		kind := model.TaskReloadSource
		if src.NeverSynced() {
			kind = model.TaskAddSource
		}
		t := snapshotTask(baseID, src, kind)
		if err := s.eng.Enqueue(r.Context(), t); err != nil {
			fail(w, err)
			return
		}
		tasks = append(tasks, t)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"tasks": tasks})
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.st.GetSource(r.Context(), r.PathValue("sid"))
	if err != nil {
		fail(w, err)
		return
	}
	t := &model.Task{BaseID: src.BaseID, SourceID: src.ID, Kind: model.TaskRemoveSource}
	if err := s.eng.Enqueue(r.Context(), t); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) sourceTree(w http.ResponseWriter, r *http.Request) {
	src, err := s.st.GetSource(r.Context(), r.PathValue("sid"))
	if err != nil {
		fail(w, err)
		return
	}
	entries, err := preview.Tree(src)
	if err != nil {
		fail(w, err)
		return
	}
	if entries == nil {
		entries = []preview.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":            src,
		"supports_chunking": model.SupportsChunking(src.Type),
		"entries":           entries,
	})
}

// ---------------------------------------------------------------- preview

func (s *Server) previewChunks(w http.ResponseWriter, r *http.Request) {
	var req preview.Request
	if !decode(w, r, &req) {
		return
	}
	res, err := preview.Run(req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type previewFileRequest struct {
	SourceID string             `json:"source_id"`
	Path     string             `json:"path"`
	Config   model.SourceConfig `json:"config"`
}

// previewFile renders one file the way ingesting it would.
//
// The path must belong to the named source. Without that check the endpoint
// reads any absolute path on the host, which on a non-loopback listener hands
// out arbitrary local files to anyone who can reach the port.
func (s *Server) previewFile(w http.ResponseWriter, r *http.Request) {
	var req previewFileRequest
	if !decode(w, r, &req) {
		return
	}
	if req.SourceID == "" {
		writeErr(w, http.StatusBadRequest, "source_id is required")
		return
	}
	src, err := s.st.GetSource(r.Context(), req.SourceID)
	if err != nil {
		fail(w, err)
		return
	}
	path, err := preview.ResolveWithinSource(src, req.Path)
	if err != nil {
		fail(w, err)
		return
	}
	if req.Config.ReaderType == "" {
		req.Config.ReaderType = src.Config.ReaderType
	}
	content, res, err := preview.PreviewFile(path, req.Config)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": content, "preview": res})
}

// ---------------------------------------------------------------- search

type searchRequest struct {
	Query       string                                 `json:"query"`
	TopK        int                                    `json:"top_k,omitempty"`
	MinScore    float64                                `json:"min_score,omitempty"`
	Mode        string                                 `json:"mode,omitempty"`
	Metadata    map[string]any                         `json:"metadata,omitempty"`
	Filter      *searchfilter.UniversalFilterCondition `json:"filter,omitempty"`
	DocumentIDs []string                               `json:"document_ids,omitempty"`
}

// upstreamNoResults is the message BuiltinKnowledge.Search returns when the
// store matched nothing. Upstream builds it with a bare fmt.Errorf and exports
// no sentinel, so matching the text is the only option; keep it in one place
// so the coupling is visible.
const upstreamNoResults = "no relevant documents found"

// searchModes maps the UI's names onto upstream's constants.
//
// SearchModeHybrid is iota, so it is also the zero value: a request that omits
// the mode runs hybrid, not vector. Naming it explicitly keeps that from being
// an accident.
var searchModes = map[string]vectorstore.SearchMode{
	"hybrid":  vectorstore.SearchModeHybrid,
	"vector":  vectorstore.SearchModeVector,
	"keyword": vectorstore.SearchModeKeyword,
	"filter":  vectorstore.SearchModeFilter,
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var req searchRequest
	if !decode(w, r, &req) {
		return
	}
	base, err := s.st.GetBase(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	mode, ok := searchModes[strings.ToLower(strings.TrimSpace(req.Mode))]
	if req.Mode != "" && !ok {
		writeErr(w, http.StatusBadRequest,
			`unknown search mode; use one of "hybrid", "vector", "keyword", "filter"`)
		return
	}
	// Only a filter-only search can run without a query.
	if strings.TrimSpace(req.Query) == "" && mode != vectorstore.SearchModeFilter {
		writeErr(w, http.StatusBadRequest, "query is required unless the mode is \"filter\"")
		return
	}
	if req.TopK <= 0 {
		req.TopK = base.Retrieval.TopK
	}
	if req.TopK > 200 {
		writeErr(w, http.StatusBadRequest, "top_k may not exceed 200")
		return
	}
	if req.MinScore < 0 || req.MinScore > 1 {
		writeErr(w, http.StatusBadRequest, "min_score must be between 0 and 1")
		return
	}

	kreq := &knowledge.SearchRequest{
		Query:      req.Query,
		MaxResults: req.TopK,
		MinScore:   req.MinScore,
		SearchMode: mode,
	}
	if len(req.Metadata) > 0 || req.Filter != nil || len(req.DocumentIDs) > 0 {
		qualifyFields(req.Filter)
		kreq.SearchFilter = &knowledge.SearchFilter{
			DocumentIDs:     req.DocumentIDs,
			Metadata:        req.Metadata,
			FilterCondition: req.Filter,
		}
	}

	start := time.Now()
	res, err := s.eng.Search(r.Context(), base.ID, kreq)
	took := time.Since(start)
	if err != nil {
		// Upstream signals an empty result set by returning an error. That is
		// not a failure — a query that matches nothing is a normal answer, and
		// surfacing it as a 500 makes the UI show a red banner for it.
		if strings.Contains(err.Error(), upstreamNoResults) {
			writeJSON(w, http.StatusOK, map[string]any{
				"results": []hit{}, "count": 0,
				"took_ms": took.Milliseconds(), "mode": modeName(mode),
				"degraded": degradedNote(base.VectorStore.Type, mode),
			})
			return
		}
		// An unreachable or misconfigured embedding endpoint is the operator's
		// configuration, not a fault in this server.
		if strings.Contains(err.Error(), "create embedding") ||
			strings.Contains(err.Error(), "embedding") && strings.Contains(err.Error(), "retrieval failed") {
			writeErr(w, http.StatusBadGateway,
				"调用 embedding 服务失败，检查 Overview & settings → Embedding 的 model 与 base URL 是否可达：\n"+
					err.Error())
			return
		}
		fail(w, err)
		return
	}
	hits := normalizeHits(res)
	writeJSON(w, http.StatusOK, map[string]any{
		"results":  hits,
		"count":    len(hits),
		"took_ms":  took.Milliseconds(),
		"mode":     modeName(mode),
		"degraded": degradedNote(base.VectorStore.Type, mode),
	})
}

// qualifyFields prefixes bare field names with source.MetadataFieldPrefix.
//
// Vector stores resolve a filter field against document metadata only when it
// carries the "metadata." prefix; a bare key matches nothing and returns an
// empty result set with no error, so a filter typed as the metadata key the UI
// itself displays would silently match zero documents. A name that already
// contains a dot is left alone so a caller can still address a top-level field.
func qualifyFields(c *searchfilter.UniversalFilterCondition) {
	if c == nil {
		return
	}
	if c.Field != "" && !strings.Contains(c.Field, ".") {
		c.Field = source.MetadataFieldPrefix + c.Field
	}
	// Logical operators nest their operands in Value.
	if nested, ok := c.Value.([]*searchfilter.UniversalFilterCondition); ok {
		for _, n := range nested {
			qualifyFields(n)
		}
	}
}

func modeName(m vectorstore.SearchMode) string {
	for k, v := range searchModes {
		if v == m {
			return k
		}
	}
	return "hybrid"
}

// degradedNote reports when the store silently does something other than what
// the chosen mode says. inmemory has no full-text index, so it answers hybrid
// with a plain vector search and keyword with a filter-only scan — both return
// results, so without saying so the UI would be quietly lying about what ran.
func degradedNote(store string, mode vectorstore.SearchMode) string {
	if store != "" && store != "inmemory" {
		return ""
	}
	switch mode {
	case vectorstore.SearchModeHybrid:
		return "inmemory 没有全文索引，hybrid 实际按纯向量检索执行"
	case vectorstore.SearchModeKeyword:
		return "inmemory 不支持关键词检索，实际退化为纯 filter 扫描，分数不代表相关度"
	}
	return ""
}

// capabilities reports what the base's vector store can actually do, so the
// retrieval UI can offer the real knobs and explain the missing ones instead
// of hiding them.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	base, err := s.st.GetBase(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	store := base.VectorStore.Type
	if store == "" {
		store = "inmemory"
	}
	inmem := store == "inmemory"
	writeJSON(w, http.StatusOK, map[string]any{
		"store": store,
		"modes": []map[string]any{
			{"value": "vector", "label": "Vector", "exact": true},
			{"value": "hybrid", "label": "Hybrid", "exact": !inmem,
				"note": degradedNote(store, vectorstore.SearchModeHybrid)},
			{"value": "keyword", "label": "Keyword", "exact": !inmem,
				"note": degradedNote(store, vectorstore.SearchModeKeyword)},
			{"value": "filter", "label": "Filter only", "exact": true,
				"note": "只按 metadata 过滤，不做相似度计算，可以不填 query"},
		},
		// RRF and its weights live on the store, not on the request: pgvector
		// and qdrant build them at construction time. There is nothing to tune
		// per query, and inmemory has no fusion at all.
		"fusion": map[string]any{
			"available": false,
			"note": "RRF 与权重是 pgvector / qdrant 在建 store 时的选项，不是查询参数；" +
				"当前 store 是 " + store + "，没有融合阶段可调。",
		},
		"operators":   filterOperators,
		"meta_prefix": source.MetaPrefix,
	})
}

// filterOperators mirrors the comparison operators upstream defines in
// knowledge/searchfilter. Listed rather than derived because the package
// exports the constants individually with no enumeration.
var filterOperators = []map[string]string{
	{"value": searchfilter.OperatorEqual, "label": "="},
	{"value": searchfilter.OperatorNotEqual, "label": "≠"},
	{"value": searchfilter.OperatorGreaterThan, "label": ">"},
	{"value": searchfilter.OperatorGreaterThanOrEqual, "label": "≥"},
	{"value": searchfilter.OperatorLessThan, "label": "<"},
	{"value": searchfilter.OperatorLessThanOrEqual, "label": "≤"},
	{"value": searchfilter.OperatorIn, "label": "in"},
	{"value": searchfilter.OperatorNotIn, "label": "not in"},
	{"value": searchfilter.OperatorLike, "label": "contains"},
	{"value": searchfilter.OperatorNotLike, "label": "not contains"},
	{"value": searchfilter.OperatorBetween, "label": "between"},
}

type hit struct {
	Name     string         `json:"name"`
	ID       string         `json:"id"`
	Score    float64        `json:"score"`
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// normalizeHits flattens the upstream search result into a stable shape.
// knowledge.SearchResult carries no JSON tags, so encoding it directly would
// expose Go field names (Document, Documents, Score) as the API contract.
func normalizeHits(res *knowledge.SearchResult) []hit {
	if res == nil {
		return []hit{}
	}
	out := []hit{}
	add := func(doc *document.Document, score float64) {
		if doc == nil {
			return
		}
		out = append(out, hit{
			Name: doc.Name, ID: doc.ID, Score: score,
			Content: doc.Content, Metadata: chunkMeta(doc.Metadata),
		})
	}
	for _, d := range res.Documents {
		if d != nil {
			add(d.Document, d.Score)
		}
	}
	// Older results only populate the single best document.
	if len(out) == 0 {
		add(res.Document, res.Score)
	}
	return out
}

// chunkMeta keeps the few keys worth showing next to a hit.
func chunkMeta(md map[string]any) map[string]any {
	if md == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{source.MetaSourceName, source.MetaURI, source.MetaChunkIndex} {
		if v, ok := md[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------- tasks

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.st.ListTasks(r.Context(), r.PathValue("id"), 50)
	if err != nil {
		fail(w, err)
		return
	}
	if tasks == nil {
		tasks = []*model.Task{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tid")
	if s.eng.Cancel(id) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceling"})
		return
	}
	writeErr(w, http.StatusConflict, "task is not running")
}

// events streams control-plane events to the browser.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	ch, unsubscribe := s.hub.Subscribe()
	defer unsubscribe()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

var _ = io.Discard
