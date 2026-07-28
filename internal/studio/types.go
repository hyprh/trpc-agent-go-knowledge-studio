//
// Copyright 2026 hyprh.
// SPDX-License-Identifier: Apache-2.0
//

package studio

import (
	"time"

	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore"
)

type previewConfig struct {
	Extractor       string `json:"extractor"`
	DoclingEndpoint string `json:"docling_endpoint"`
	OutputFormat    string `json:"output_format"`
	OCREnabled      bool   `json:"ocr_enabled"`
	ImageMode       string `json:"image_mode"`
	Reader          string `json:"reader"`
	Strategy        string `json:"strategy"`
	ChunkSize       int    `json:"chunk_size"`
	Overlap         int    `json:"overlap"`
}

type previewResponse struct {
	ID        string         `json:"id"`
	Input     inputView      `json:"input"`
	Config    previewConfig  `json:"config"`
	Extracted extractedView  `json:"extracted"`
	Summary   previewSummary `json:"summary"`
	Chunks    []chunkView    `json:"chunks"`
	CreatedAt time.Time      `json:"created_at"`
	Warnings  []string       `json:"warnings,omitempty"`
}

type inputView struct {
	Name        string `json:"name"`
	Bytes       int    `json:"bytes"`
	Runes       int    `json:"runes"`
	ContentType string `json:"content_type"`
	Text        string `json:"text,omitempty"`
}

type extractedView struct {
	Applied bool   `json:"applied"`
	Format  string `json:"format"`
	Content string `json:"content"`
	Bytes   int    `json:"bytes"`
	Runes   int    `json:"runes"`
}

type previewSummary struct {
	Reader       string  `json:"reader"`
	Strategy     string  `json:"strategy"`
	SizeUnit     string  `json:"size_unit"`
	Total        int     `json:"total"`
	MinSize      int     `json:"min_size"`
	MaxSize      int     `json:"max_size"`
	AverageSize  float64 `json:"average_size"`
	OverBudget   int     `json:"over_budget"`
	MappedChunks int     `json:"mapped_chunks"`
}

type chunkView struct {
	Index         int            `json:"index"`
	ID            string         `json:"id"`
	Content       string         `json:"content"`
	EmbeddingText string         `json:"embedding_text,omitempty"`
	Runes         int            `json:"runes"`
	Bytes         int            `json:"bytes"`
	WithinBudget  bool           `json:"within_budget"`
	ActualOverlap int            `json:"actual_overlap"`
	Metadata      map[string]any `json:"metadata"`
	Mapped        bool           `json:"mapped"`
	SourceStart   int            `json:"source_start"`
	SourceEnd     int            `json:"source_end"`
	OverlapStart  int            `json:"overlap_start"`
}

type storedPreview struct {
	response  *previewResponse
	documents []*document.Document
	expiresAt time.Time
}

type indexRequest struct {
	PreviewID  string         `json:"preview_id"`
	Store      storeConfig    `json:"store"`
	Embedder   embedderConfig `json:"embedder"`
	TopK       int            `json:"top_k"`
	Recreate   bool           `json:"recreate"`
	SourceSync bool           `json:"source_sync"`
}

type storeConfig struct {
	Type             string `json:"type"`
	DSN              string `json:"dsn"`
	Table            string `json:"table"`
	Dimensions       int    `json:"dimensions"`
	EnableTextSearch bool   `json:"enable_text_search"`
	Fusion           string `json:"fusion"`
}

type embedderConfig struct {
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
}

type indexResponse struct {
	ID            string    `json:"id"`
	PreviewID     string    `json:"preview_id"`
	Store         string    `json:"store"`
	Table         string    `json:"table,omitempty"`
	Documents     int       `json:"documents"`
	DurationMS    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
	SearchModes   []string  `json:"search_modes"`
	FrameworkPath string    `json:"framework_path"`
}

type indexSession struct {
	id        string
	previewID string
	kb        *knowledge.BuiltinKnowledge
	store     vectorstore.VectorStore
	storeType string
	table     string
	count     int
	createdAt time.Time
}

type searchRequest struct {
	IndexID    string         `json:"index_id"`
	Query      string         `json:"query"`
	Mode       string         `json:"mode"`
	MaxResults int            `json:"max_results"`
	MinScore   float64        `json:"min_score"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type searchResponse struct {
	Query      string         `json:"query"`
	Mode       string         `json:"mode"`
	DurationMS int64          `json:"duration_ms"`
	Results    []searchResult `json:"results"`
}

type searchResult struct {
	Rank        int                `json:"rank"`
	Score       float64            `json:"score"`
	DenseScore  any                `json:"dense_score,omitempty"`
	SparseScore any                `json:"sparse_score,omitempty"`
	Document    *document.Document `json:"document"`
}
