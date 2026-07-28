// Package model defines the control-plane data model for the workbench.
//
// The control plane is the sole authority on source configuration: the
// upstream source.Source interface exposes only Name/Type/GetMetadata and
// cannot be queried for the configuration it was built with, so nothing in
// this package can be reconciled from a running knowledge base.
package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SourceType enumerates the upstream source kinds.
type SourceType string

// Source types, mirroring knowledge/source.Type*.
const (
	SourceAuto SourceType = "auto"
	SourceFile SourceType = "file"
	SourceDir  SourceType = "dir"
	SourceURL  SourceType = "url"
	SourceRepo SourceType = "repo"
)

// Strategy enumerates the chunking strategies.
type Strategy string

// Chunking strategies.
const (
	StrategyRecursive Strategy = "recursive"
	StrategyFixed     Strategy = "fixed"
	StrategyMarkdown  Strategy = "markdown"
	StrategyJSON      Strategy = "json"
)

// ReaderType enumerates the file reader types. Values match
// knowledge/source.FileReaderType* exactly — note Go is "go", not "golang".
type ReaderType string

// Reader types.
const (
	ReaderText     ReaderType = "text"
	ReaderMarkdown ReaderType = "markdown"
	ReaderJSON     ReaderType = "json"
	ReaderCSV      ReaderType = "csv"
	ReaderPDF      ReaderType = "pdf"
	ReaderDocx     ReaderType = "docx"
	ReaderProto    ReaderType = "proto"
	ReaderGo       ReaderType = "go"
)

// SourceState is the lifecycle state of a source row.
type SourceState string

// Source states.
const (
	SourceDraft    SourceState = "draft"
	SourceActive   SourceState = "active"
	SourceDisabled SourceState = "disabled"
)

// TaskKind enumerates control-plane operations that mutate a knowledge base.
type TaskKind string

// Task kinds.
const (
	TaskAddSource    TaskKind = "add_source"
	TaskReloadSource TaskKind = "reload_source"
	TaskRemoveSource TaskKind = "remove_source"
	TaskLoadAll      TaskKind = "load_all"
)

// TaskState is the lifecycle state of a task.
type TaskState string

// Task states.
const (
	TaskQueued    TaskState = "queued"
	TaskRunning   TaskState = "running"
	TaskSucceeded TaskState = "succeeded"
	TaskFailed    TaskState = "failed"
	TaskCanceled  TaskState = "canceled"
)

// Reserved metadata prefixes. User metadata must never use them: upstream
// deletes a source's vectors by filtering on trpc_agent_go_source_name, so a
// user-supplied override of that key would break removal permanently.
const (
	ReservedPrefixAgent = "trpc_agent_go_"
	ReservedPrefixAST   = "trpc_ast_"
)

// ErrValidation is the root of every configuration error in this package.
// Callers map it onto 422 rather than 500: a rejected configuration is the
// user's to fix, not a server fault. Every error returned by Validate and
// ValidateMetadata wraps it.
var ErrValidation = errors.New("invalid configuration")

// Errors returned by validation. Each wraps ErrValidation.
var (
	ErrInvalidChunkSize = fmt.Errorf("%w: chunk size must be positive", ErrValidation)
	ErrInvalidOverlap   = fmt.Errorf("%w: overlap must not be negative", ErrValidation)
	ErrOverlapTooLarge  = fmt.Errorf("%w: overlap must be smaller than chunk size", ErrValidation)
	ErrReservedMetadata = fmt.Errorf("%w: metadata key uses a reserved prefix", ErrValidation)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

// MinCoreBudget is the minimum amount of new content a chunk must carry after
// overlap is reserved. Widening the core to satisfy a tight budget is what
// produces chunks of chunkSize+overlap, so configurations below this are
// rejected rather than silently adjusted.
const MinCoreBudget = 16

// RepoConfig mirrors knowledge/source/repo.Repository.
type RepoConfig struct {
	URL         string `json:"url,omitempty"`
	Dir         string `json:"dir,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Tag         string `json:"tag,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Subdir      string `json:"subdir,omitempty"`
	RepoName    string `json:"repo_name,omitempty"`
	Description string `json:"description,omitempty"`
	RepoURL     string `json:"repo_url,omitempty"`
}

// SourceConfig carries every per-type option. Which fields are legal depends
// on the source type; Validate enforces the discrimination. Notably the repo
// type has no chunking options at all — it performs AST extraction rather
// than text chunking — so accepting chunk_size on a repo source would silently
// do nothing.
type SourceConfig struct {
	// Chunking, for file/dir/url/auto only.
	ChunkSize    int        `json:"chunk_size,omitempty"`
	ChunkOverlap int        `json:"chunk_overlap,omitempty"`
	ReaderType   ReaderType `json:"reader_type,omitempty"`
	Strategy     Strategy   `json:"strategy,omitempty"`

	// Per-type inputs.
	Paths  []string `json:"paths,omitempty"`  // file
	Dirs   []string `json:"dirs,omitempty"`   // dir
	URLs   []string `json:"urls,omitempty"`   // url
	Inputs []string `json:"inputs,omitempty"` // auto

	// dir only.
	Recursive *bool `json:"recursive,omitempty"`

	// dir and repo.
	FileExtensions []string `json:"file_extensions,omitempty"`

	// repo only.
	Repository       *RepoConfig `json:"repository,omitempty"`
	DocExtensions    []string    `json:"doc_extensions,omitempty"`
	SkipDirs         []string    `json:"skip_dirs,omitempty"`
	SkipSuffixes     []string    `json:"skip_suffixes,omitempty"`
	ParseConcurrency int         `json:"parse_concurrency,omitempty"`
}

// SupportsChunking reports whether a source type accepts chunking options.
func SupportsChunking(t SourceType) bool { return t != SourceRepo }

// Validate checks the configuration against its declared type.
func (c *SourceConfig) Validate(t SourceType) error {
	if SupportsChunking(t) {
		if err := c.ValidateChunking(); err != nil {
			return err
		}
	} else if c.ChunkSize != 0 || c.ChunkOverlap != 0 || c.ReaderType != "" || c.Strategy != "" {
		return invalid("source type %q performs AST extraction and accepts no chunking options", t)
	}

	switch t {
	case SourceFile:
		if len(c.Paths) == 0 {
			return invalid("file source requires at least one path")
		}
	case SourceDir:
		if len(c.Dirs) == 0 {
			return invalid("dir source requires at least one directory")
		}
	case SourceURL:
		if len(c.URLs) == 0 {
			return invalid("url source requires at least one url")
		}
	case SourceAuto:
		if len(c.Inputs) == 0 {
			return invalid("auto source requires at least one input")
		}
	case SourceRepo:
		if c.Repository == nil || (c.Repository.URL == "" && c.Repository.Dir == "") {
			return invalid("repo source requires repository.url or repository.dir")
		}
	default:
		return invalid("unknown source type %q", t)
	}

	// Reject inputs that belong to a different type, so a config edit that
	// changes the type cannot leave stale fields behind.
	type field struct {
		name  string
		empty bool
		ok    bool
	}
	for _, f := range []field{
		{"paths", len(c.Paths) == 0, t == SourceFile},
		{"dirs", len(c.Dirs) == 0, t == SourceDir},
		{"urls", len(c.URLs) == 0, t == SourceURL},
		{"inputs", len(c.Inputs) == 0, t == SourceAuto},
		{"repository", c.Repository == nil, t == SourceRepo},
		{"recursive", c.Recursive == nil, t == SourceDir},
		{"file_extensions", len(c.FileExtensions) == 0, t == SourceDir || t == SourceRepo},
		{"doc_extensions", len(c.DocExtensions) == 0, t == SourceRepo},
		{"skip_dirs", len(c.SkipDirs) == 0, t == SourceRepo},
		{"skip_suffixes", len(c.SkipSuffixes) == 0, t == SourceRepo},
		{"parse_concurrency", c.ParseConcurrency == 0, t == SourceRepo},
	} {
		if !f.empty && !f.ok {
			return invalid("field %q is not valid for source type %q", f.name, t)
		}
	}
	return nil
}

// ValidateChunking checks only the chunking parameters. The chunk preview uses
// it directly: previewing operates on content that is already in hand, so
// requiring the source's input paths would reject a perfectly valid request.
func (c *SourceConfig) ValidateChunking() error {
	if c.ChunkSize <= 0 {
		return ErrInvalidChunkSize
	}
	if c.ChunkOverlap < 0 {
		return ErrInvalidOverlap
	}
	if c.ChunkOverlap >= c.ChunkSize {
		return ErrOverlapTooLarge
	}
	if c.ChunkSize-c.ChunkOverlap < MinCoreBudget {
		return invalid("only %d units of new content remain after overlap, need at least %d",
			c.ChunkSize-c.ChunkOverlap, MinCoreBudget)
	}
	switch c.Strategy {
	case StrategyRecursive, StrategyFixed, StrategyMarkdown, StrategyJSON, "":
	default:
		return invalid("unknown chunking strategy %q", c.Strategy)
	}
	switch c.ReaderType {
	case ReaderText, ReaderMarkdown, ReaderJSON, ReaderCSV, ReaderPDF, ReaderDocx, ReaderProto, ReaderGo, "":
	default:
		return invalid("unknown reader type %q", c.ReaderType)
	}
	return nil
}

// ValidateMetadata rejects user metadata that would collide with the keys
// upstream injects and depends on.
func ValidateMetadata(md map[string]any) error {
	for k := range md {
		if strings.HasPrefix(k, ReservedPrefixAgent) || strings.HasPrefix(k, ReservedPrefixAST) {
			return fmt.Errorf("%w: %q", ErrReservedMetadata, k)
		}
		if k == "" {
			return invalid("metadata key must not be empty")
		}
	}
	return nil
}

// EmbedderConfig describes the embedding endpoint for a knowledge base.
type EmbedderConfig struct {
	Provider   string `json:"provider"` // "openai" (OpenAI-compatible) — also covers Ollama
	BaseURL    string `json:"base_url,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	Model      string `json:"model,omitempty"`
	Dimensions int    `json:"dimensions,omitempty"`
}

// Configured reports whether the embedder is usable.
func (e EmbedderConfig) Configured() bool { return e.Model != "" }

// VectorStoreConfig describes the vector backend.
type VectorStoreConfig struct {
	Type string `json:"type"` // "inmemory" — others not wired yet
}

// Persistent reports whether vectors written to this store outlive the
// process. The in-memory store does not, so any record of a completed ingest
// is stale the moment the workbench restarts.
func (v VectorStoreConfig) Persistent() bool {
	return v.Type != "" && v.Type != "inmemory"
}

// SourceDefaults pre-fills the new-source form. It is deliberately not applied
// at runtime: upstream has no notion of defaults, every source.Source carries a
// complete configuration. Inheriting at runtime would let a defaults edit
// silently change the chunking of every source that never overrode it, with no
// visible change on the source itself.
type SourceDefaults struct {
	ReaderType   ReaderType `json:"reader_type,omitempty"`
	Strategy     Strategy   `json:"strategy"`
	ChunkSize    int        `json:"chunk_size"`
	ChunkOverlap int        `json:"chunk_overlap"`
}

// RetrievalConfig holds the base's default retrieval parameters.
type RetrievalConfig struct {
	TopK int `json:"top_k"`
}

// Base is a knowledge base.
type Base struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	VectorStore      VectorStoreConfig `json:"vector_store"`
	Embedder         EmbedderConfig    `json:"embedder"`
	EnableSourceSync bool              `json:"enable_source_sync"`
	SourceDefaults   SourceDefaults    `json:"source_defaults"`
	Retrieval        RetrievalConfig   `json:"retrieval"`
	IndexRevision    int               `json:"index_revision"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`

	// Derived, not stored.
	SourceCount int `json:"source_count"`
	DocCount    int `json:"doc_count"`
	ChunkCount  int `json:"chunk_count"`
}

// Source is a configured knowledge source.
//
// ID doubles as the upstream Source.Name(). It is immutable: upstream deletes
// a source's vectors with a filter on that name, so changing it would orphan
// every vector already written.
type Source struct {
	ID               string         `json:"id"`
	BaseID           string         `json:"base_id"`
	DisplayName      string         `json:"display_name"`
	Type             SourceType     `json:"type"`
	Config           SourceConfig   `json:"config"`
	ConfigHash       string         `json:"config_hash"`
	SyncedConfigHash string         `json:"synced_config_hash"`
	UserMetadata     map[string]any `json:"user_metadata,omitempty"`
	State            SourceState    `json:"state"`
	LastSyncedAt     *time.Time     `json:"last_synced_at,omitempty"`
	DocCount         int            `json:"doc_count"`
	ChunkCount       int            `json:"chunk_count"`
	LastError        string         `json:"last_error,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// NeverSynced reports whether the source has no vectors upstream.
func (s *Source) NeverSynced() bool { return s.SyncedConfigHash == "" }

// NeedsSync reports whether the stored configuration differs from what was
// last successfully ingested. It is derived rather than stored so the two can
// never disagree.
func (s *Source) NeedsSync() bool { return s.ConfigHash != s.SyncedConfigHash }

// SyncStatus renders the derived state for the UI.
func (s *Source) SyncStatus() string {
	switch {
	case s.LastError != "":
		return "failed"
	case s.NeverSynced():
		return "never_synced"
	case s.NeedsSync():
		return "stale"
	default:
		return "synced"
	}
}

// Counters is a snapshot of a base's volume, taken before and after a task so
// deltas can be reported. Load progress events carry document counts only, so
// chunk deltas have to be measured this way.
type Counters struct {
	Docs   int `json:"docs"`
	Chunks int `json:"chunks"`
}

// Progress mirrors knowledge.LoadProgressEvent.
type Progress struct {
	SourceName      string `json:"source_name,omitempty"`
	SourceProcessed int    `json:"source_processed"`
	SourceTotal     int    `json:"source_total"`
	SourceElapsedMS int64  `json:"source_elapsed_ms"`
	SourceETAMS     int64  `json:"source_eta_ms"`
	Total           int    `json:"total"`
	TotalElapsedMS  int64  `json:"total_elapsed_ms"`
	Done            bool   `json:"done"`
}

// Task is a queued or completed control-plane operation.
type Task struct {
	ID             string       `json:"id"`
	BaseID         string       `json:"base_id"`
	SourceID       string       `json:"source_id,omitempty"`
	Kind           TaskKind     `json:"kind"`
	State          TaskState    `json:"state"`
	RequestedBy    string       `json:"requested_by"`
	Params         SourceConfig `json:"params"`
	Progress       Progress     `json:"progress"`
	CountersBefore Counters     `json:"counters_before"`
	CountersAfter  Counters     `json:"counters_after"`
	Error          string       `json:"error,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	StartedAt      *time.Time   `json:"started_at,omitempty"`
	FinishedAt     *time.Time   `json:"finished_at,omitempty"`
}

// Terminal reports whether the task will not change state again.
func (t *Task) Terminal() bool {
	return t.State == TaskSucceeded || t.State == TaskFailed || t.State == TaskCanceled
}

var idSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

// Slug renders a human-readable, lowercase identifier fragment.
func Slug(s string) string {
	s = idSanitizer.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}
