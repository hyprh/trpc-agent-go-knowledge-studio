//
// Copyright 2026 hyprh.
// SPDX-License-Identifier: Apache-2.0
//

package studio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestBody         = 10 << 20
	maxPreviewContent      = 8 << 20
	maxConcurrentPreviews  = 2
	maxConcurrentIndexRuns = 1
	previewTTL             = 30 * time.Minute
	maxPreviewSessions     = 16
	maxIndexSessions       = 8
)

type Server struct {
	webFS    fs.FS
	sampleFS fs.FS

	mu       sync.RWMutex
	previews map[string]*storedPreview
	indexes  map[string]*indexSession

	previewSlots chan struct{}
	indexSlots   chan struct{}
}

func New(webFS, sampleFS fs.FS) *Server {
	return &Server{
		webFS:        webFS,
		sampleFS:     sampleFS,
		previews:     make(map[string]*storedPreview),
		indexes:      make(map[string]*indexSession),
		previewSlots: make(chan struct{}, maxConcurrentPreviews),
		indexSlots:   make(chan struct{}, maxConcurrentIndexRuns),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.api(s.handleHealth))
	mux.HandleFunc("/api/capabilities", s.api(s.handleCapabilities))
	mux.HandleFunc("/api/samples", s.api(s.handleSamples))
	mux.HandleFunc("/api/preview", s.api(s.handlePreview))
	mux.HandleFunc("/api/index", s.api(s.handleIndex))
	mux.HandleFunc("/api/search", s.api(s.handleSearch))
	mux.Handle("/", s.securityHeaders(http.FileServer(http.FS(s.webFS))))
	return mux
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, session := range s.indexes {
		if session.store != nil {
			if err := session.store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close index %s: %w", id, err))
			}
		}
	}
	s.indexes = make(map[string]*indexSession)
	s.previews = make(map[string]*storedPreview)
	return errors.Join(errs...)
}

func (s *Server) api(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.cleanupExpiredPreviews()
		s.securityHeaders(next).ServeHTTP(w, r)
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set(
			"Content-Security-Policy",
			"default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' blob: data:; connect-src 'self'",
		)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cleanupExpiredPreviews() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, preview := range s.previews {
		if now.After(preview.expiresAt) {
			delete(s.previews, id)
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"previews": len(s.previews),
		"indexes":  len(s.indexes),
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources":    []string{"upload", "text"},
		"extractors": []string{"direct", "docling"},
		"readers":    []string{"auto", "text", "markdown", "json", "csv", "proto"},
		"strategies": []string{"reader", "fixed", "recursive", "markdown", "json"},
		"stores":     []string{"memory", "pgvector"},
		"embedders":  []string{"openai-compatible"},
		"search_modes": []string{
			"vector", "keyword", "hybrid", "filter",
		},
		"limits": map[string]any{
			"max_input_bytes":        maxPreviewContent,
			"max_concurrent_preview": maxConcurrentPreviews,
			"max_concurrent_index":   maxConcurrentIndexRuns,
			"max_preview_sessions":   maxPreviewSessions,
			"max_index_sessions":     maxIndexSessions,
		},
	})
}

type sampleItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	entries, err := fs.ReadDir(s.sampleFS, ".")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read samples")
		return
	}
	samples := make([]sampleItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		content, err := fs.ReadFile(s.sampleFS, entry.Name())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read sample "+entry.Name())
			return
		}
		samples = append(samples, sampleItem{
			ID:          entry.Name(),
			Name:        entry.Name(),
			Description: sampleDescription(entry.Name()),
			Content:     string(content),
		})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Name < samples[j].Name })
	writeJSON(w, http.StatusOK, samples)
}

func sampleDescription(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md":
		return "Markdown headings, tables, long paragraphs, code blocks, and multilingual boundaries."
	case ".json":
		return "Nested JSON with numeric and lexical keys for deterministic structural chunking."
	case ".csv":
		return "Quoted commas, embedded newlines, empty fields, long records, and UTF-8 content."
	default:
		return "Plain text with short sentences, long paragraphs, punctuation, and mixed scripts."
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}
