//
// Copyright 2026 hyprh.
// SPDX-License-Identifier: Apache-2.0
//

package studio

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/chunking"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/csv"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/json"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/markdown"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/proto"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/text"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/extractor"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/extractor/docling"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
)

const (
	defaultChunkSize = 500
	maxChunkCount    = 5000
	maxChunkBytes    = 16 << 20
	minChunkSize     = 16
)

var sourceMappingTokenPattern = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_.:/-]{3,}`)

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	select {
	case s.previewSlots <- struct{}{}:
		defer func() { <-s.previewSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "preview workers are busy; retry shortly")
		return
	}

	input, cfg, err := parsePreviewRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	response, docs, err := runPreview(ctx, input, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.registerPreview(response.ID, &storedPreview{
		response:  response,
		documents: cloneDocuments(docs),
		expiresAt: time.Now().Add(previewTTL),
	})
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) registerPreview(id string, preview *storedPreview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.previews) >= maxPreviewSessions {
		var oldestID string
		var oldest time.Time
		for existingID, item := range s.previews {
			if oldestID == "" || item.response.CreatedAt.Before(oldest) {
				oldestID = existingID
				oldest = item.response.CreatedAt
			}
		}
		delete(s.previews, oldestID)
	}
	s.previews[id] = preview
}

type previewInput struct {
	name        string
	contentType string
	data        []byte
}

func parsePreviewRequest(w http.ResponseWriter, r *http.Request) (*previewInput, previewConfig, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := r.ParseMultipartForm(maxPreviewContent); err != nil {
		return nil, previewConfig{}, fmt.Errorf("parse upload: %w", err)
	}
	var cfg previewConfig
	configJSON := r.FormValue("config")
	if configJSON == "" {
		return nil, cfg, fmt.Errorf("config is required")
	}
	decoder := json.NewDecoder(strings.NewReader(configJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, cfg, fmt.Errorf("decode config: %w", err)
	}
	if err := validatePreviewConfig(&cfg); err != nil {
		return nil, cfg, err
	}

	data, name, contentType, err := readPreviewInput(r.MultipartForm)
	if err != nil {
		return nil, cfg, err
	}
	if len(data) == 0 {
		return nil, cfg, fmt.Errorf("input must not be empty")
	}
	if len(data) > maxPreviewContent {
		return nil, cfg, fmt.Errorf("input exceeds %d bytes", maxPreviewContent)
	}
	if name == "" {
		name = "document.txt"
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	return &previewInput{name: filepath.Base(name), contentType: contentType, data: data}, cfg, nil
}

func readPreviewInput(form *multipart.Form) ([]byte, string, string, error) {
	files := form.File["file"]
	if len(files) > 1 {
		return nil, "", "", fmt.Errorf("upload exactly one file")
	}
	if len(files) == 1 {
		file, err := files[0].Open()
		if err != nil {
			return nil, "", "", fmt.Errorf("open upload: %w", err)
		}
		defer file.Close()
		data, err := readLimited(file, maxPreviewContent)
		if err != nil {
			return nil, "", "", fmt.Errorf("read upload: %w", err)
		}
		return data, files[0].Filename, files[0].Header.Get("Content-Type"), nil
	}
	content := ""
	if values := form.Value["content"]; len(values) > 0 {
		content = values[0]
	}
	name := "document.md"
	if values := form.Value["name"]; len(values) > 0 && strings.TrimSpace(values[0]) != "" {
		name = values[0]
	}
	return []byte(content), name, "text/plain; charset=utf-8", nil
}

func validatePreviewConfig(cfg *previewConfig) error {
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.ChunkSize < minChunkSize {
		return fmt.Errorf("chunk size must be at least %d", minChunkSize)
	}
	if cfg.Overlap < 0 {
		return fmt.Errorf("overlap must be non-negative")
	}
	if cfg.Overlap >= cfg.ChunkSize {
		return fmt.Errorf("overlap must be smaller than chunk size")
	}
	switch cfg.Extractor {
	case "", "direct":
		cfg.Extractor = "direct"
	case "docling":
		if cfg.DoclingEndpoint == "" {
			cfg.DoclingEndpoint = "http://127.0.0.1:5001"
		}
		parsed, err := url.Parse(cfg.DoclingEndpoint)
		if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("docling endpoint must be an http(s) URL")
		}
	default:
		return fmt.Errorf("unsupported extractor %q", cfg.Extractor)
	}
	switch cfg.OutputFormat {
	case "", extractor.FormatMarkdown:
		cfg.OutputFormat = extractor.FormatMarkdown
	case extractor.FormatText:
	default:
		return fmt.Errorf("unsupported extractor output format %q", cfg.OutputFormat)
	}
	switch cfg.ImageMode {
	case "", string(docling.ImageRefModePlaceholder):
		cfg.ImageMode = string(docling.ImageRefModePlaceholder)
	case string(docling.ImageRefModeEmbedded), string(docling.ImageRefModeReferenced):
	default:
		return fmt.Errorf("unsupported image mode %q", cfg.ImageMode)
	}
	switch cfg.Reader {
	case "", "auto":
		cfg.Reader = "auto"
	case "text", "markdown", "json", "csv", "proto":
	default:
		return fmt.Errorf("unsupported reader %q", cfg.Reader)
	}
	switch cfg.Strategy {
	case "", "reader":
		cfg.Strategy = "reader"
	case "fixed", "recursive", "markdown", "json":
	default:
		return fmt.Errorf("unsupported strategy %q", cfg.Strategy)
	}
	if cfg.Strategy == "json" {
		cfg.Overlap = 0
	}
	return nil
}

func runPreview(ctx context.Context, input *previewInput, cfg previewConfig) (*previewResponse, []*document.Document, error) {
	extracted, format, applied, err := extractContent(ctx, input, cfg)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(extracted) == "" {
		return nil, nil, fmt.Errorf("extractor returned empty content")
	}
	if !utf8.ValidString(extracted) {
		return nil, nil, fmt.Errorf("reader input is not valid UTF-8; use an extractor for binary content")
	}

	previewID := newID("preview")
	docs, readerName, strategyName, err := chunkContent(previewID, input.name, extracted, format, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("chunk document: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil, fmt.Errorf("reader produced no documents")
	}
	if len(docs) > maxChunkCount {
		return nil, nil, fmt.Errorf("chunk count %d exceeds limit %d", len(docs), maxChunkCount)
	}
	totalBytes := 0
	for _, doc := range docs {
		totalBytes += len(doc.Content)
	}
	if totalBytes > maxChunkBytes {
		return nil, nil, fmt.Errorf("chunk output exceeds %d bytes", maxChunkBytes)
	}

	normalized := normalizeSource(extracted)
	chunks, summary := buildChunkViews(normalized, docs, cfg, readerName, strategyName)
	var warnings []string
	if summary.MappedChunks < len(chunks) {
		warnings = append(warnings,
			"Some structural chunks cannot be mapped to one contiguous source range; this is expected for JSON and repeated Markdown context.",
		)
	}

	rawText := ""
	if utf8.Valid(input.data) &&
		(strings.HasPrefix(input.contentType, "text/") || isTextExtension(filepath.Ext(input.name))) {
		rawText = string(input.data)
	}
	response := &previewResponse{
		ID: previewID,
		Input: inputView{
			Name:        input.name,
			Bytes:       len(input.data),
			Runes:       utf8.RuneCount(input.data),
			ContentType: input.contentType,
			Text:        rawText,
		},
		Config: cfg,
		Extracted: extractedView{
			Applied: applied,
			Format:  format,
			Content: normalized,
			Bytes:   len(normalized),
			Runes:   utf8.RuneCountInString(normalized),
		},
		Summary:   summary,
		Chunks:    chunks,
		CreatedAt: time.Now().UTC(),
		Warnings:  warnings,
	}
	return response, docs, nil
}

func extractContent(
	ctx context.Context,
	input *previewInput,
	cfg previewConfig,
) (content, format string, applied bool, err error) {
	if cfg.Extractor == "direct" {
		return string(input.data), formatForName(input.name), false, nil
	}
	ext := docling.New(
		docling.WithEndpoint(cfg.DoclingEndpoint),
		docling.WithOCR(cfg.OCREnabled),
		docling.WithImageRefMode(docling.ImageRefMode(cfg.ImageMode)),
		docling.WithTimeout(3*time.Minute),
	)
	defer ext.Close()
	result, err := ext.Extract(
		ctx,
		input.data,
		extractor.WithOutputFormat(cfg.OutputFormat),
	)
	if err != nil {
		return "", "", true, fmt.Errorf("docling extraction failed: %w", err)
	}
	contentBytes, err := readLimited(result.Reader, maxPreviewContent)
	if err != nil {
		return "", "", true, fmt.Errorf("read docling output: %w", err)
	}
	return string(contentBytes), result.Format, true, nil
}

func chunkContent(
	previewID, name, content, extractedFormat string,
	cfg previewConfig,
) ([]*document.Document, string, string, error) {
	doc := &document.Document{
		ID:      previewID,
		Name:    name,
		Content: content,
		Metadata: map[string]any{
			source.MetaSource:     source.TypeFile,
			source.MetaFileName:   name,
			source.MetaFileExt:    filepath.Ext(name),
			source.MetaURI:        "studio://" + previewID + "/" + url.PathEscape(name),
			source.MetaSourceName: "Knowledge Studio Preview",
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if cfg.Strategy != "reader" {
		strategy, err := strategyFor(cfg.Strategy, cfg.ChunkSize, cfg.Overlap)
		if err != nil {
			return nil, "", "", err
		}
		docs, err := strategy.Chunk(doc)
		return docs, "DirectDocument", cfg.Strategy, err
	}

	readerType, extension := resolveReader(cfg.Reader, name, extractedFormat)
	var opts []reader.Option
	if readerType != "proto" {
		strategy, err := defaultReaderStrategy(readerType, cfg.ChunkSize, cfg.Overlap)
		if err != nil {
			return nil, "", "", err
		}
		opts = append(opts, reader.WithCustomChunkingStrategy(strategy))
	}
	r, ok := reader.GetReader(extension, opts...)
	if !ok {
		return nil, "", "", fmt.Errorf("no reader registered for %s", extension)
	}
	docs, err := r.ReadFromReader(name, bytes.NewReader([]byte(content)))
	if err != nil {
		return nil, "", "", err
	}
	for _, item := range docs {
		if item.Metadata == nil {
			item.Metadata = make(map[string]any)
		}
		for key, value := range doc.Metadata {
			item.Metadata[key] = value
		}
	}
	return docs, r.Name(), "reader:" + readerType, nil
}

func strategyFor(name string, chunkSize, overlap int) (chunking.Strategy, error) {
	switch name {
	case "fixed":
		return chunking.NewFixedSizeChunking(
			chunking.WithChunkSize(chunkSize),
			chunking.WithOverlap(overlap),
		), nil
	case "recursive":
		return chunking.NewRecursiveChunking(
			chunking.WithRecursiveChunkSize(chunkSize),
			chunking.WithRecursiveOverlap(overlap),
		), nil
	case "markdown":
		return chunking.NewMarkdownChunking(
			chunking.WithMarkdownChunkSize(chunkSize),
			chunking.WithMarkdownOverlap(overlap),
		), nil
	case "json":
		return chunking.NewJSONChunking(chunking.WithJSONChunkSize(chunkSize)), nil
	default:
		return nil, fmt.Errorf("unsupported chunking strategy %q", name)
	}
}

func defaultReaderStrategy(readerType string, chunkSize, overlap int) (chunking.Strategy, error) {
	switch readerType {
	case "markdown":
		return strategyFor("markdown", chunkSize, overlap)
	case "json":
		return strategyFor("json", chunkSize, 0)
	default:
		return strategyFor("fixed", chunkSize, overlap)
	}
}

func resolveReader(configured, name, extractedFormat string) (string, string) {
	if configured != "auto" {
		return configured, extensionForReader(configured)
	}
	if extractedFormat == extractor.FormatMarkdown {
		return "markdown", ".md"
	}
	if extractedFormat == extractor.FormatText {
		return "text", ".txt"
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "markdown", ".md"
	case ".json":
		return "json", ".json"
	case ".csv":
		return "csv", ".csv"
	case ".proto":
		return "proto", ".proto"
	default:
		return "text", ".txt"
	}
}

func extensionForReader(name string) string {
	switch name {
	case "markdown":
		return ".md"
	case "json":
		return ".json"
	case "csv":
		return ".csv"
	case "proto":
		return ".proto"
	default:
		return ".txt"
	}
}

func formatForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return extractor.FormatMarkdown
	default:
		return "source"
	}
}

func isTextExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".txt", ".text", ".md", ".markdown", ".json", ".csv", ".proto", ".go", ".py", ".yaml", ".yml", ".html":
		return true
	default:
		return false
	}
}

func buildChunkViews(
	sourceContent string,
	docs []*document.Document,
	cfg previewConfig,
	readerName, strategyName string,
) ([]chunkView, previewSummary) {
	sizeUnit := "runes"
	if cfg.Strategy == "json" || strings.Contains(strategyName, "json") {
		sizeUnit = "bytes"
	}
	views := make([]chunkView, 0, len(docs))
	summary := previewSummary{
		Reader:   readerName,
		Strategy: strategyName,
		SizeUnit: sizeUnit,
		Total:    len(docs),
	}
	totalSize := 0
	for i, doc := range docs {
		size := utf8.RuneCountInString(doc.Content)
		if sizeUnit == "bytes" {
			size = len(doc.Content)
		}
		if i == 0 || size < summary.MinSize {
			summary.MinSize = size
		}
		if size > summary.MaxSize {
			summary.MaxSize = size
		}
		totalSize += size
		if size > cfg.ChunkSize {
			summary.OverBudget++
		}
		actualOverlap := 0
		if i > 0 {
			actualOverlap = boundaryOverlap(docs[i-1].Content, doc.Content, cfg.Overlap)
		}
		views = append(views, chunkView{
			Index:         i + 1,
			ID:            doc.ID,
			Content:       doc.Content,
			EmbeddingText: doc.EmbeddingText,
			Runes:         utf8.RuneCountInString(doc.Content),
			Bytes:         len(doc.Content),
			WithinBudget:  size <= cfg.ChunkSize,
			ActualOverlap: actualOverlap,
			Metadata:      cloneMap(doc.Metadata),
			OverlapStart:  actualOverlap,
		})
	}
	if len(docs) > 0 {
		summary.AverageSize = float64(totalSize) / float64(len(docs))
	}
	summary.MappedChunks = mapSourceRanges(sourceContent, views)
	return views, summary
}

func mapSourceRanges(content string, chunks []chunkView) int {
	cursor := 0
	mapped := 0
	tokenIndex := buildSourceTokenIndex(content)
	for i := range chunks {
		runes := []rune(chunks[i].Content)
		overlap := min(chunks[i].ActualOverlap, len(runes))

		// Prefer the complete chunk. This preserves the source range of real
		// overlap produced by Fixed and Recursive chunking.
		overlapBytes := len(string(runes[:overlap]))
		fullSearchFrom := max(0, cursor-overlapBytes)
		if offset := indexNear(content, fullSearchFrom, chunks[i].Content); offset >= 0 {
			startByte := fullSearchFrom + offset
			endByte := startByte + len(chunks[i].Content)
			chunks[i].Mapped = true
			chunks[i].SourceStart = utf8.RuneCountInString(content[:startByte])
			chunks[i].SourceEnd = utf8.RuneCountInString(content[:endByte])
			chunks[i].OverlapStart = chunks[i].SourceStart + overlap
			cursor = endByte
			mapped++
			continue
		}

		// Structural strategies can repeat headings or reshape whitespace.
		// Map their non-overlap core when the whole chunk is not contiguous.
		core := strings.TrimSpace(string(runes[overlap:]))
		if core == "" {
			continue
		}
		searchFrom := min(cursor, len(content))
		offset := indexNear(content, searchFrom, core)
		if offset < 0 {
			startByte, endByte, ok := mapByDistinctiveToken(content, chunks[i].Content, tokenIndex)
			if !ok {
				continue
			}
			chunks[i].Mapped = true
			chunks[i].SourceStart = utf8.RuneCountInString(content[:startByte])
			chunks[i].SourceEnd = utf8.RuneCountInString(content[:endByte])
			chunks[i].OverlapStart = chunks[i].SourceStart
			mapped++
			continue
		}
		startByte := searchFrom + offset
		endByte := startByte + len(core)
		chunks[i].Mapped = true
		chunks[i].SourceStart = utf8.RuneCountInString(content[:startByte])
		chunks[i].SourceEnd = utf8.RuneCountInString(content[:endByte])
		chunks[i].OverlapStart = chunks[i].SourceStart
		cursor = endByte
		mapped++
	}
	return mapped
}

type sourceTokenLocation struct {
	offset int
	count  int
}

func buildSourceTokenIndex(content string) map[string]sourceTokenLocation {
	result := make(map[string]sourceTokenLocation)
	for _, indexes := range sourceMappingTokenPattern.FindAllStringIndex(content, -1) {
		token := content[indexes[0]:indexes[1]]
		location := result[token]
		if location.count == 0 {
			location.offset = indexes[0]
		}
		location.count++
		result[token] = location
	}
	return result
}

func mapByDistinctiveToken(
	content, chunk string,
	tokenIndex map[string]sourceTokenLocation,
) (int, int, bool) {
	tokens := sourceMappingTokenPattern.FindAllString(chunk, -1)
	sort.SliceStable(tokens, func(i, j int) bool {
		return utf8.RuneCountInString(tokens[i]) > utf8.RuneCountInString(tokens[j])
	})
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		location, ok := tokenIndex[token]
		if !ok || location.count != 1 {
			continue
		}
		offset := location.offset
		lineStart := strings.LastIndex(content[:offset], "\n") + 1
		lineEndOffset := strings.Index(content[offset:], "\n")
		lineEnd := len(content)
		if lineEndOffset >= 0 {
			lineEnd = offset + lineEndOffset
		}
		return lineStart, lineEnd, true
	}
	return 0, 0, false
}

func indexNear(content string, start int, needle string) int {
	const mappingLookahead = 64 << 10
	if start < 0 || start > len(content) || needle == "" {
		return -1
	}
	end := min(len(content), start+len(needle)+mappingLookahead)
	return strings.Index(content[start:end], needle)
}

func boundaryOverlap(previous, current string, limit int) int {
	if limit <= 0 {
		return 0
	}
	left := []rune(previous)
	right := []rune(current)
	maxSize := min(limit, min(len(left), len(right)))
	for size := maxSize; size > 0; size-- {
		if string(left[len(left)-size:]) == string(right[:size]) {
			return size
		}
	}
	return 0
}

func normalizeSource(content string) string {
	content = strings.TrimSpace(content)
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}

func readLimited(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("content exceeds %d bytes", limit)
	}
	return data, nil
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}

func cloneDocuments(docs []*document.Document) []*document.Document {
	result := make([]*document.Document, 0, len(docs))
	for _, doc := range docs {
		result = append(result, doc.Clone())
	}
	return result
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
