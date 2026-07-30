// Package preview runs the real chunking strategies over a document without
// touching a knowledge base. It needs no embedder and no vector store, so the
// chunk inspector works before anything is configured.
package preview

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/document"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/sourcefactory"
)

// MaxContentBytes bounds a previewed document.
const MaxContentBytes = 1 << 20

// MaxChunks bounds the response size.
const MaxChunks = 5000

// Chunk is one previewed chunk.
type Chunk struct {
	Index   int    `json:"index"`
	Content string `json:"content"`
	Size    int    `json:"size"`
	Unit    string `json:"unit"`
	// OverlapSize is how much of the chunk's head is carried over from the
	// previous chunk, in the active unit. Upstream records the core size and
	// the total separately; the difference is the prepended overlap.
	OverlapSize int            `json:"overlap_size"`
	OverBudget  bool           `json:"over_budget"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Result is a preview response.
type Result struct {
	Unit     string `json:"unit"`
	Strategy string `json:"strategy"`
	// ResolvedFrom is non-empty when the strategy was not chosen explicitly but
	// derived from the reader, so the UI can say which default was applied.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	// Reader names the reader that actually parsed the file, so the inspector
	// can say which transform produced these chunks.
	Reader string `json:"reader,omitempty"`
	// Kind tells the UI how to lay the content out: "csv", "json" or "text".
	Kind       string  `json:"kind,omitempty"`
	ChunkSize  int     `json:"chunk_size"`
	Overlap    int     `json:"chunk_overlap"`
	SourceSize int     `json:"source_size"`
	Count      int     `json:"count"`
	AvgSize    int     `json:"avg_size"`
	MaxSize    int     `json:"max_size"`
	OverBudget int     `json:"over_budget"`
	Truncated  bool    `json:"truncated"`
	Chunks     []Chunk `json:"chunks"`
}

// Request describes what to preview.
type Request struct {
	Content    string             `json:"content"`
	Name       string             `json:"name"`
	Config     model.SourceConfig `json:"config"`
	SourceKind model.SourceType   `json:"source_type"`
}

// Run chunks the content with the configured strategy.
func Run(req Request) (*Result, error) {
	if req.SourceKind == "" {
		req.SourceKind = model.SourceFile
	}
	if !model.SupportsChunking(req.SourceKind) {
		return nil, fmt.Errorf("source type %q performs AST extraction; there is nothing to preview", req.SourceKind)
	}
	if len(req.Content) > MaxContentBytes {
		return nil, fmt.Errorf("document is %d bytes, the preview limit is %d", len(req.Content), MaxContentBytes)
	}
	if strings.TrimSpace(req.Content) == "" {
		return nil, errors.New("document is empty")
	}
	// Only the chunking parameters matter here: the content is already in
	// hand, so the source's input paths are irrelevant to a preview.
	if err := req.Config.ValidateChunking(); err != nil {
		return nil, err
	}

	cfg := req.Config
	strategy := sourcefactory.Strategy(cfg)

	doc := &document.Document{
		ID:      req.Name,
		Name:    req.Name,
		Content: req.Content,
		Metadata: map[string]any{
			source.MetaSourceName: "preview",
			source.MetaURI:        req.Name,
		},
	}
	// Chunk the content as given. This path is for text pasted into the
	// inspector, where there is no file for a reader to parse; PreviewFile
	// runs the real reader instead.
	if strategy == nil {
		cfg.Strategy = DefaultStrategyFor(cfg.ReaderType)
		strategy = sourcefactory.Strategy(cfg)
	}
	chunks, err := strategy.Chunk(doc)
	if err != nil {
		return nil, err
	}
	resolvedFrom := ""
	if req.Config.Strategy == "" {
		resolvedFrom = "reader default"
	}

	unit := "runes"
	if cfg.Strategy == model.StrategyJSON {
		unit = "bytes"
	}
	sizeOf := func(s string) int {
		if unit == "bytes" {
			return len(s)
		}
		return utf8.RuneCountInString(s)
	}

	res := &Result{
		Unit:         unit,
		Strategy:     string(cfg.Strategy),
		ResolvedFrom: resolvedFrom,
		ChunkSize:    cfg.ChunkSize,
		Overlap:      cfg.ChunkOverlap,
		SourceSize:   sizeOf(req.Content),
	}
	total := 0
	for i, c := range chunks {
		if i >= MaxChunks {
			res.Truncated = true
			break
		}
		n := sizeOf(c.Content)
		over := n > cfg.ChunkSize
		if over {
			res.OverBudget++
		}
		if n > res.MaxSize {
			res.MaxSize = n
		}
		total += n
		md := pickMetadata(c.Metadata)
		prev := ""
		if i > 0 {
			prev = chunks[i-1].Content
		}
		res.Chunks = append(res.Chunks, Chunk{
			Index:       i,
			Content:     c.Content,
			Size:        n,
			Unit:        unit,
			OverlapSize: sharedWithPrev(prev, c.Content, overlapOf(md, n), unit),
			OverBudget:  over,
			Metadata:    md,
		})
	}
	res.Count = len(res.Chunks)
	if res.Count > 0 {
		res.AvgSize = total / res.Count
	}
	return res, nil
}

// sharedWithPrev narrows the metadata-derived overlap to the text actually
// repeated from the previous chunk.
//
// Upstream builds a chunk as suffix(previous) + separator + core, and records
// only the totals, so total-core counts the separator as overlap too. The
// separator is not repeated content — highlighting it as such puts a newline
// or two inside the shared region on every chunk. The real value is at most a
// few units below the recorded one, so walking down a short window finds it
// without scanning the whole chunk.
func sharedWithPrev(prev, cur string, recorded int, unit string) int {
	if recorded <= 0 || prev == "" || cur == "" {
		return 0
	}
	const window = 8 // longest separator upstream inserts, plus slack
	if unit == "bytes" {
		p, c := []byte(prev), []byte(cur)
		for k := minInt(recorded, len(p), len(c)); k > 0 && k > recorded-window; k-- {
			if string(p[len(p)-k:]) == string(c[:k]) {
				return k
			}
		}
		return recorded
	}
	p, c := []rune(prev), []rune(cur)
	for k := minInt(recorded, len(p), len(c)); k > 0 && k > recorded-window; k-- {
		if string(p[len(p)-k:]) == string(c[:k]) {
			return k
		}
	}
	return recorded
}

func minInt(vs ...int) int {
	m := vs[0]
	for _, v := range vs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

// PreviewFile chunks a file the way ingesting it actually would.
//
// It runs the registered reader for the file's extension rather than feeding
// raw bytes to a chunking strategy. That distinction is the whole point of the
// inspector: readers transform before they chunk. The CSV reader normalises
// records to pipe-separated fields, the Go and Proto readers extract AST
// entities, and PDF and DOCX are not text at all. Chunking the raw bytes
// produced different content and a different chunk count from the same file —
// which made the one screen people tune parameters on disagree with what the
// ingest would do.
func PreviewFile(path string, cfg model.SourceConfig) (string, *Result, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory", filepath.Base(path))
	}
	if info.Size() > MaxContentBytes {
		return "", nil, fmt.Errorf("file is %d bytes, the preview limit is %d",
			info.Size(), MaxContentBytes)
	}
	if err := cfg.ValidateChunking(); err != nil {
		return "", nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	// An explicit reader type overrides the extension, exactly as
	// WithFileReaderType does on a source.
	if cfg.ReaderType != "" {
		if e, ok := extForReader(cfg.ReaderType); ok {
			ext = e
		}
	}
	rd, ok := reader.GetReader(ext, readerOptions(cfg)...)
	if !ok {
		return "", nil, fmt.Errorf(
			"没有能处理 %q 的 reader；已注册的扩展名: %s", ext,
			strings.Join(reader.GetRegisteredExtensions(), " "))
	}

	docs, err := rd.ReadFromFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("reader %s: %w", rd.Name(), err)
	}

	// The source pane shows the file as it is on disk when that is text, so
	// boundaries can be located in it. Binary formats have no such view.
	raw := ""
	if b, err := os.ReadFile(path); err == nil && utf8.Valid(b) {
		raw = string(b)
	}
	return raw, summarise(docs, cfg, rd.Name()), nil
}

// readerOptions maps the stored config onto the reader's own options, so the
// preview is configured exactly the way a source would configure it.
func readerOptions(cfg model.SourceConfig) []reader.Option {
	opts := []reader.Option{
		reader.WithChunkSize(cfg.ChunkSize),
		reader.WithChunkOverlap(cfg.ChunkOverlap),
	}
	if st := sourcefactory.Strategy(cfg); st != nil {
		opts = append(opts, reader.WithCustomChunkingStrategy(st))
	}
	return opts
}

// strategyForReader names the chunking each upstream reader builds when no
// override is given. Taken from the readers themselves: text and CSV construct
// FixedSizeChunking, Markdown constructs MarkdownChunking, JSON constructs
// JSONChunking.
func strategyForReader(name string) model.Strategy {
	switch {
	case strings.HasPrefix(name, "JSON"):
		return model.StrategyJSON
	case strings.HasPrefix(name, "Markdown"):
		return model.StrategyMarkdown
	case strings.HasPrefix(name, "CSV"), strings.HasPrefix(name, "Text"):
		return model.StrategyFixed
	default:
		return model.StrategyRecursive
	}
}

// kindForReader tells the UI how to display chunk content. Reader output is not
// always prose: the CSV reader emits pipe-separated records and the JSON reader
// emits re-serialized objects, both of which are far easier to compare against
// the source when laid out rather than shown as one long line.
func kindForReader(name string) string {
	switch {
	case strings.HasPrefix(name, "CSV"):
		return "csv"
	case strings.HasPrefix(name, "JSON"):
		return "json"
	case strings.HasPrefix(name, "Markdown"):
		return "markdown"
	default:
		return "text"
	}
}

// extForReader maps a reader type back to an extension the registry knows.
func extForReader(r model.ReaderType) (string, bool) {
	switch r {
	case model.ReaderText:
		return ".txt", true
	case model.ReaderMarkdown:
		return ".md", true
	case model.ReaderJSON:
		return ".json", true
	case model.ReaderCSV:
		return ".csv", true
	case model.ReaderPDF:
		return ".pdf", true
	case model.ReaderDocx:
		return ".docx", true
	case model.ReaderProto:
		return ".proto", true
	case model.ReaderGo:
		return ".go", true
	}
	return "", false
}

// summarise turns reader output into the inspector's view of it.
func summarise(chunks []*document.Document, cfg model.SourceConfig, readerName string) *Result {
	// Report the strategy that ran, not the one that was configured. With no
	// override the reader builds its own, so labelling the result from an empty
	// config called every unconfigured preview "recursive" and measured JSON in
	// runes when JSONChunking budgets in serialized bytes.
	effective := cfg.Strategy
	resolved := ""
	if effective == "" {
		effective = strategyForReader(readerName)
		resolved = readerName + " default"
	}
	unit := "runes"
	if effective == model.StrategyJSON {
		unit = "bytes"
	}
	sizeOf := func(s string) int {
		if unit == "bytes" {
			return len(s)
		}
		return utf8.RuneCountInString(s)
	}
	res := &Result{
		Unit:         unit,
		Strategy:     string(effective),
		Reader:       readerName,
		Kind:         kindForReader(readerName),
		ChunkSize:    cfg.ChunkSize,
		Overlap:      cfg.ChunkOverlap,
		ResolvedFrom: resolved,
	}
	total := 0
	for i, c := range chunks {
		if i >= MaxChunks {
			res.Truncated = true
			break
		}
		n := sizeOf(c.Content)
		over := n > cfg.ChunkSize
		if over {
			res.OverBudget++
		}
		if n > res.MaxSize {
			res.MaxSize = n
		}
		total += n
		res.SourceSize += n
		md := pickMetadata(c.Metadata)
		prev := ""
		if i > 0 {
			prev = chunks[i-1].Content
		}
		res.Chunks = append(res.Chunks, Chunk{
			Index:       i,
			Content:     c.Content,
			Size:        n,
			Unit:        unit,
			OverlapSize: sharedWithPrev(prev, c.Content, overlapOf(md, n), unit),
			OverBudget:  over,
			Metadata:    md,
		})
	}
	res.Count = len(res.Chunks)
	if res.Count > 0 {
		res.AvgSize = total / res.Count
	}
	return res
}

// overlapOf derives how much of a chunk's head is repeated from the previous
// chunk. Upstream records the core size under MetaChunkSize and the total
// under MetaOverlappedContentSize, and only sets the latter when overlap was
// actually prepended; the difference between them is the overlap.
func overlapOf(md map[string]any, total int) int {
	if md == nil {
		return 0
	}
	whole, ok := toInt(md[source.MetaOverlappedContentSize])
	if !ok {
		return 0
	}
	core, ok := toInt(md[source.MetaChunkSize])
	if !ok {
		return 0
	}
	n := whole - core
	if n <= 0 || n > total {
		return 0
	}
	return n
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// pickMetadata returns the chunk's metadata as the reader produced it.
//
// It used to keep a five-key whitelist to bound the response, but every key a
// reader attaches is something the inspector exists to show — a filter written
// against a key the UI never displayed is the reason metadata filters silently
// matched nothing. Only genuinely large values are trimmed, and the trim is
// visible rather than silent.
func pickMetadata(md map[string]any) map[string]any {
	if len(md) == 0 {
		return nil
	}
	const maxValueLen = 400
	out := make(map[string]any, len(md))
	for k, v := range md {
		if s, ok := v.(string); ok && len(s) > maxValueLen {
			out[k] = s[:maxValueLen] + fmt.Sprintf("… (+%d 字符)", len(s)-maxValueLen)
			continue
		}
		out[k] = v
	}
	return out
}

// TreeEntry is one inspectable file under a source.
type TreeEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Ext  string `json:"ext"`
}

// MaxTreeEntries bounds directory expansion.
const MaxTreeEntries = 500

// Tree lists the files a source would read, for the inspector's left column.
// It walks the filesystem directly rather than calling ReadDocuments, which
// would parse and chunk every file just to render a list.
func Tree(src *model.Source) ([]TreeEntry, error) {
	var roots []string
	switch src.Type {
	case model.SourceFile:
		roots = src.Config.Paths
	case model.SourceDir:
		roots = src.Config.Dirs
	case model.SourceAuto:
		roots = src.Config.Inputs
	case model.SourceRepo:
		if src.Config.Repository != nil && src.Config.Repository.Dir != "" {
			roots = []string{src.Config.Repository.Dir}
		}
	case model.SourceURL:
		out := make([]TreeEntry, 0, len(src.Config.URLs))
		for _, u := range src.Config.URLs {
			out = append(out, TreeEntry{Path: u, Name: u, Ext: ".url"})
		}
		return out, nil
	}

	allow := map[string]bool{}
	for _, e := range src.Config.FileExtensions {
		allow[strings.ToLower(e)] = true
	}
	recursive := src.Type != model.SourceDir || src.Config.Recursive == nil || *src.Config.Recursive

	var out []TreeEntry
	seen := map[string]bool{}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			addEntry(&out, seen, root, info.Size(), allow)
			continue
		}
		err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(out) >= MaxTreeEntries {
				return filepath.SkipAll
			}
			if d.IsDir() {
				if p != root && !recursive {
					return filepath.SkipDir
				}
				if name := d.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			addEntry(&out, seen, p, fi.Size(), allow)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func addEntry(out *[]TreeEntry, seen map[string]bool, path string, size int64, allow map[string]bool) {
	if seen[path] {
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	if len(allow) > 0 && !allow[ext] {
		return
	}
	seen[path] = true
	*out = append(*out, TreeEntry{Path: path, Name: filepath.Base(path), Size: size, Ext: ext})
}

// ResolveWithinSource confines a requested path to the roots a source declares.
//
// Both the roots and the request are resolved through symlinks before they are
// compared, so a link inside a source directory cannot be used to step outside
// it. Without this the preview endpoint is an arbitrary-file-read primitive for
// anyone who can reach the port.
func ResolveWithinSource(src *model.Source, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: path is required", model.ErrValidation)
	}
	roots := sourceRoots(src)
	if len(roots) == 0 {
		return "", fmt.Errorf("%w: source %q declares no readable paths", model.ErrValidation, src.DisplayName)
	}
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		// Report the original path; the resolved one leaks layout on failure.
		return "", fmt.Errorf("%w: %s", fs.ErrNotExist, filepath.Base(path))
	}
	for _, root := range roots {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if want == real {
			return want, nil
		}
		// filepath.Rel alone accepts "..", so require the prefix as well.
		rel, err := filepath.Rel(real, want)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return want, nil
	}
	return "", fmt.Errorf("%w: 该路径不属于 source %q", model.ErrValidation, src.DisplayName)
}

// sourceRoots lists the filesystem locations a source is allowed to read.
func sourceRoots(src *model.Source) []string {
	switch src.Type {
	case model.SourceFile:
		return src.Config.Paths
	case model.SourceDir:
		return src.Config.Dirs
	case model.SourceAuto:
		return src.Config.Inputs
	case model.SourceRepo:
		if src.Config.Repository != nil && src.Config.Repository.Dir != "" {
			return []string{src.Config.Repository.Dir}
		}
	}
	return nil
}

// ReadFile loads a file for preview, refusing anything over the limit.
func ReadFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > MaxContentBytes {
		return "", fmt.Errorf("file is %d bytes, the preview limit is %d", info.Size(), MaxContentBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", errors.New("file is not valid UTF-8 and cannot be previewed as text")
	}
	return string(b), nil
}

// ReaderForExt guesses the reader type from a file extension, matching what
// the auto source would pick.
func ReaderForExt(ext string) model.ReaderType {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return model.ReaderMarkdown
	case ".json":
		return model.ReaderJSON
	case ".csv":
		return model.ReaderCSV
	case ".go":
		return model.ReaderGo
	case ".proto":
		return model.ReaderProto
	case ".pdf":
		return model.ReaderPDF
	case ".docx":
		return model.ReaderDocx
	default:
		return model.ReaderText
	}
}

// DefaultStrategyFor returns the chunking strategy a reader uses when none is
// configured, mirroring what each upstream reader constructs for itself:
// text and CSV build FixedSizeChunking, Markdown builds MarkdownChunking, JSON
// builds JSONChunking. Go and Proto extract AST entities and do not chunk text
// at all; previewing them as text is the closest honest approximation, so they
// fall back to recursive.
func DefaultStrategyFor(r model.ReaderType) model.Strategy {
	switch r {
	case model.ReaderMarkdown:
		return model.StrategyMarkdown
	case model.ReaderJSON:
		return model.StrategyJSON
	case model.ReaderText, model.ReaderCSV, model.ReaderPDF, model.ReaderDocx:
		return model.StrategyFixed
	default:
		return model.StrategyRecursive
	}
}
