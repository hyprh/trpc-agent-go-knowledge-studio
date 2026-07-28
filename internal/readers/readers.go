// Package readers registers every document reader with the upstream registry.
//
// Readers register themselves from init(), so a reader that is never imported
// simply does not exist as far as the registry is concerned. That failure is
// silent and expensive: GetFileType(".go") consults the registry and quietly
// downgrades to the text reader when the Go reader is absent, and an explicit
// reader_type of "go" fails outright with "no reader available for file type",
// which upstream logs as a per-file warning and then reports the load as
// successful with zero documents.
//
// Import this package for side effects from main so the set of readers the UI
// offers is the set that actually exists.
package readers

import (
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/csv"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/docx"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/golang"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/json"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/markdown"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/pdf"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/proto"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/python"
	_ "trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader/text"

	"trpc.group/trpc-go/trpc-agent-go/knowledge/document/reader"
)

// Extensions returns the file extensions that currently have a reader, so the
// UI can offer only reader types that will actually resolve.
func Extensions() []string { return reader.GetRegisteredExtensions() }
