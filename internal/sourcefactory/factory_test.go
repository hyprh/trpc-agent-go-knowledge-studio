package sourcefactory

import (
	"strings"
	"testing"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
)

func ptr[T any](v T) *T { return &v }

func TestBuildNamesSourceByID(t *testing.T) {
	// The upstream name must be the immutable id, not the display name:
	// RemoveSource deletes vectors by filtering on this value.
	src := &model.Source{
		ID:          "src_abc123",
		DisplayName: "人类可读的名字",
		Type:        model.SourceFile,
		Config: model.SourceConfig{
			Paths: []string{"/tmp/x.md"}, ChunkSize: 1024, ChunkOverlap: 128,
			Strategy: model.StrategyRecursive,
		},
	}
	obj, err := Build(src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := obj.Name(); got != "src_abc123" {
		t.Fatalf("Name() = %q, want the source id", got)
	}
}

func TestBuildEveryType(t *testing.T) {
	cases := []struct {
		name string
		src  *model.Source
		want string
	}{
		{"file", &model.Source{ID: "s1", Type: model.SourceFile, Config: model.SourceConfig{
			Paths: []string{"/tmp/a.md"}, ChunkSize: 512, ChunkOverlap: 64, Strategy: model.StrategyMarkdown,
			ReaderType: model.ReaderMarkdown}}, "file"},
		{"dir", &model.Source{ID: "s2", Type: model.SourceDir, Config: model.SourceConfig{
			Dirs: []string{"/tmp"}, ChunkSize: 1024, ChunkOverlap: 0, Strategy: model.StrategyRecursive,
			FileExtensions: []string{".go"}, Recursive: ptr(true), ReaderType: model.ReaderGo}}, "dir"},
		{"url", &model.Source{ID: "s3", Type: model.SourceURL, Config: model.SourceConfig{
			URLs: []string{"https://example.com"}, ChunkSize: 512, Strategy: model.StrategyFixed}}, "url"},
		{"auto", &model.Source{ID: "s4", Type: model.SourceAuto, Config: model.SourceConfig{
			Inputs: []string{"/tmp"}, ChunkSize: 1024, ChunkOverlap: 128, Strategy: model.StrategyRecursive}}, "auto"},
		{"repo", &model.Source{ID: "s5", Type: model.SourceRepo, Config: model.SourceConfig{
			Repository: &model.RepoConfig{URL: "github.com/x/y", Branch: "main"},
			SkipDirs:   []string{"vendor"}, ParseConcurrency: 2}}, "repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := Build(tc.src)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if obj.Type() != tc.want {
				t.Fatalf("Type() = %q, want %q", obj.Type(), tc.want)
			}
			if obj.Name() != tc.src.ID {
				t.Fatalf("Name() = %q, want %q", obj.Name(), tc.src.ID)
			}
		})
	}
}

// Repo sources perform AST extraction and have no chunking options upstream.
// Accepting chunk_size would silently do nothing, so it must be rejected.
func TestRepoRejectsChunkingOptions(t *testing.T) {
	src := &model.Source{ID: "s", Type: model.SourceRepo, Config: model.SourceConfig{
		Repository: &model.RepoConfig{URL: "github.com/x/y"},
		ChunkSize:  1024,
	}}
	_, err := Build(src)
	if err == nil {
		t.Fatal("expected repo source with chunk_size to be rejected")
	}
	if !strings.Contains(err.Error(), "no chunking options") {
		t.Fatalf("error should explain why: %v", err)
	}
}

func TestCrossTypeFieldsRejected(t *testing.T) {
	// A type change that leaves the old type's fields behind must not build.
	src := &model.Source{ID: "s", Type: model.SourceFile, Config: model.SourceConfig{
		Paths: []string{"/tmp/a.md"}, Dirs: []string{"/tmp"},
		ChunkSize: 1024, ChunkOverlap: 128, Strategy: model.StrategyRecursive,
	}}
	if _, err := Build(src); err == nil {
		t.Fatal("expected dirs on a file source to be rejected")
	}
}

func TestReservedMetadataRejected(t *testing.T) {
	// Overriding trpc_agent_go_source_name would permanently break removal.
	for _, key := range []string{"trpc_agent_go_source_name", "trpc_ast_type"} {
		src := &model.Source{ID: "s", Type: model.SourceFile, Config: model.SourceConfig{
			Paths: []string{"/tmp/a.md"}, ChunkSize: 1024, ChunkOverlap: 128,
			Strategy: model.StrategyRecursive,
		}, UserMetadata: map[string]any{key: "hijacked"}}
		if _, err := Build(src); err == nil {
			t.Fatalf("expected reserved metadata key %q to be rejected", key)
		}
	}
}

func TestChunkingValidation(t *testing.T) {
	base := func(size, overlap int) *model.SourceConfig {
		return &model.SourceConfig{Paths: []string{"/tmp/a"}, ChunkSize: size,
			ChunkOverlap: overlap, Strategy: model.StrategyRecursive}
	}
	cases := []struct {
		name          string
		size, overlap int
		wantErr       error
	}{
		{"ok", 1024, 128, nil},
		{"zero size", 0, 0, model.ErrInvalidChunkSize},
		{"negative overlap", 1024, -1, model.ErrInvalidOverlap},
		{"overlap equals size", 128, 128, model.ErrOverlapTooLarge},
		{"overlap exceeds size", 128, 200, model.ErrOverlapTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := base(tc.size, tc.overlap).Validate(model.SourceFile)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && err == nil {
				t.Fatalf("expected %v", tc.wantErr)
			}
		})
	}
	// A budget that leaves too little new content is rejected rather than
	// silently widened, which is what produces size+overlap chunks.
	if err := base(128, 120).Validate(model.SourceFile); err == nil {
		t.Fatal("expected a too-tight core budget to be rejected")
	}
}
