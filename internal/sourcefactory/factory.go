// Package sourcefactory builds upstream source.Source objects from stored
// configuration.
//
// This is the only place in the workbench that knows the upstream option
// functions. Everything else deals in model.SourceConfig.
package sourcefactory

import (
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/chunking"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
	autosource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/auto"
	dirsource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/dir"
	filesource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/file"
	reposource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/repo"
	urlsource "trpc.group/trpc-go/trpc-agent-go/knowledge/source/url"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
)

// Build constructs an upstream source from a stored row.
//
// The returned source's Name() is src.ID, never the display name. Upstream
// deletes a source's vectors by filtering on that name, so a mutable name
// would orphan every vector already written the first time someone renames a
// source in the UI.
func Build(src *model.Source) (source.Source, error) {
	if err := src.Config.Validate(src.Type); err != nil {
		return nil, fmt.Errorf("invalid config for source %s: %w", src.ID, err)
	}
	if err := model.ValidateMetadata(src.UserMetadata); err != nil {
		return nil, err
	}
	switch src.Type {
	case model.SourceFile:
		return buildFile(src), nil
	case model.SourceDir:
		return buildDir(src), nil
	case model.SourceURL:
		return buildURL(src), nil
	case model.SourceAuto:
		return buildAuto(src), nil
	case model.SourceRepo:
		return buildRepo(src), nil
	default:
		return nil, fmt.Errorf("unknown source type %q", src.Type)
	}
}

func buildFile(src *model.Source) source.Source {
	c := src.Config
	opts := []filesource.Option{
		filesource.WithName(src.ID),
		filesource.WithChunkSize(c.ChunkSize),
		filesource.WithChunkOverlap(c.ChunkOverlap),
	}
	if c.ReaderType != "" {
		opts = append(opts, filesource.WithFileReaderType(source.FileReaderType(c.ReaderType)))
	}
	if st := strategy(c); st != nil {
		opts = append(opts, filesource.WithCustomChunkingStrategy(st))
	}
	if len(src.UserMetadata) > 0 {
		opts = append(opts, filesource.WithMetadata(src.UserMetadata))
	}
	return filesource.New(c.Paths, opts...)
}

func buildDir(src *model.Source) source.Source {
	c := src.Config
	opts := []dirsource.Option{
		dirsource.WithName(src.ID),
		dirsource.WithChunkSize(c.ChunkSize),
		dirsource.WithChunkOverlap(c.ChunkOverlap),
	}
	if c.ReaderType != "" {
		opts = append(opts, dirsource.WithFileReaderType(source.FileReaderType(c.ReaderType)))
	}
	if st := strategy(c); st != nil {
		opts = append(opts, dirsource.WithCustomChunkingStrategy(st))
	}
	if len(c.FileExtensions) > 0 {
		opts = append(opts, dirsource.WithFileExtensions(c.FileExtensions))
	}
	if c.Recursive != nil {
		opts = append(opts, dirsource.WithRecursive(*c.Recursive))
	}
	if len(src.UserMetadata) > 0 {
		opts = append(opts, dirsource.WithMetadata(src.UserMetadata))
	}
	return dirsource.New(c.Dirs, opts...)
}

func buildURL(src *model.Source) source.Source {
	c := src.Config
	opts := []urlsource.Option{
		urlsource.WithName(src.ID),
		urlsource.WithChunkSize(c.ChunkSize),
		urlsource.WithChunkOverlap(c.ChunkOverlap),
	}
	if c.ReaderType != "" {
		opts = append(opts, urlsource.WithFileReaderType(source.FileReaderType(c.ReaderType)))
	}
	if st := strategy(c); st != nil {
		opts = append(opts, urlsource.WithCustomChunkingStrategy(st))
	}
	if len(src.UserMetadata) > 0 {
		opts = append(opts, urlsource.WithMetadata(src.UserMetadata))
	}
	return urlsource.New(c.URLs, opts...)
}

func buildAuto(src *model.Source) source.Source {
	c := src.Config
	opts := []autosource.Option{
		autosource.WithName(src.ID),
		autosource.WithChunkSize(c.ChunkSize),
		autosource.WithChunkOverlap(c.ChunkOverlap),
	}
	if c.ReaderType != "" {
		opts = append(opts, autosource.WithFileReaderType(source.FileReaderType(c.ReaderType)))
	}
	if st := strategy(c); st != nil {
		opts = append(opts, autosource.WithCustomChunkingStrategy(st))
	}
	if len(src.UserMetadata) > 0 {
		opts = append(opts, autosource.WithMetadata(src.UserMetadata))
	}
	return autosource.New(c.Inputs, opts...)
}

// buildRepo has no chunking options: the repo source performs AST extraction
// rather than text chunking, so there is nothing to size.
func buildRepo(src *model.Source) source.Source {
	c := src.Config
	r := c.Repository
	opts := []reposource.Option{
		reposource.WithName(src.ID),
		reposource.WithRepository(reposource.Repository{
			URL:         r.URL,
			Dir:         r.Dir,
			Branch:      r.Branch,
			Tag:         r.Tag,
			Commit:      r.Commit,
			Subdir:      r.Subdir,
			RepoName:    r.RepoName,
			Description: r.Description,
			RepoURL:     r.RepoURL,
		}),
	}
	if len(c.FileExtensions) > 0 {
		opts = append(opts, reposource.WithFileExtensions(c.FileExtensions))
	}
	if len(c.DocExtensions) > 0 {
		opts = append(opts, reposource.WithDocExtensions(c.DocExtensions))
	}
	if len(c.SkipDirs) > 0 {
		opts = append(opts, reposource.WithSkipDirs(c.SkipDirs))
	}
	if len(c.SkipSuffixes) > 0 {
		opts = append(opts, reposource.WithSkipSuffixes(c.SkipSuffixes))
	}
	if c.ParseConcurrency > 0 {
		opts = append(opts, reposource.WithParseConcurrency(c.ParseConcurrency))
	}
	if len(src.UserMetadata) > 0 {
		opts = append(opts, reposource.WithMetadata(src.UserMetadata))
	}
	return reposource.New(opts...)
}

// strategy maps a stored strategy name onto an upstream chunking strategy.
// It returns nil for the empty value so the source keeps its own default.
func strategy(c model.SourceConfig) chunking.Strategy {
	switch c.Strategy {
	case model.StrategyFixed:
		return chunking.NewFixedSizeChunking(
			chunking.WithChunkSize(c.ChunkSize),
			chunking.WithOverlap(c.ChunkOverlap),
		)
	case model.StrategyRecursive:
		return chunking.NewRecursiveChunking(
			chunking.WithRecursiveChunkSize(c.ChunkSize),
			chunking.WithRecursiveOverlap(c.ChunkOverlap),
		)
	case model.StrategyMarkdown:
		return chunking.NewMarkdownChunking(
			chunking.WithMarkdownChunkSize(c.ChunkSize),
			chunking.WithMarkdownOverlap(c.ChunkOverlap),
		)
	case model.StrategyJSON:
		// JSON sizes are measured in serialized bytes, not runes, and the
		// strategy takes no overlap option.
		return chunking.NewJSONChunking(chunking.WithJSONChunkSize(c.ChunkSize))
	default:
		return nil
	}
}

// Strategy exposes the strategy mapping for the stateless chunk preview,
// which builds a strategy without building a whole source.
func Strategy(c model.SourceConfig) chunking.Strategy { return strategy(c) }
