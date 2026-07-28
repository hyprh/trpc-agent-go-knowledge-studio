# Knowledge Studio

Knowledge Studio is a local-first workbench for inspecting and tuning the
`trpc-agent-go` Knowledge pipeline. It exposes the real reader, extractor,
chunking, vector-store, and retrieval paths instead of maintaining a separate
browser-only implementation.

This repository is an early runnable prototype. Its current focus is the path
from one document to an explainable retrieval result:

1. Choose a built-in sample, edit text, or upload a document.
2. Use the document directly or extract it through Docling Serve.
3. Select a reader and chunking strategy, then inspect chunk boundaries,
   overlap, source mapping, and metadata.
4. Load the exact preview snapshot into InMemory or PGVector.
5. Run vector, keyword, hybrid, or metadata-filtered retrieval and inspect the
   final, dense, and sparse scores.

![Pipeline view](docs/pipeline-view.svg)

## Run locally

Go 1.21 or newer is required.

```bash
go run .
```

Open <http://127.0.0.1:8090>. The server listens only on loopback by default.
Use `-addr` to opt into another address.

```bash
go run . -addr 127.0.0.1:9000
```

The Direct pipeline and chunk preview require no external services.

### PGVector

PGVector is the default persistent store in the UI. Start the bundled
PostgreSQL service:

```bash
docker compose up -d postgres
```

The pre-filled DSN is:

```text
postgres://postgres:postgres@127.0.0.1:5432/knowledge_studio?sslmode=disable
```

The selected embedding dimension becomes the PGVector column dimension. Use a
new table name when changing dimensions, or explicitly enable **Delete existing
rows** when rebuilding a compatible table.

### Embeddings

Retrieval needs an OpenAI-compatible embeddings endpoint. For OpenAI, leave the
base URL empty, set `OPENAI_API_KEY`, and use an embedding model with matching
dimensions.

A local Ollama endpoint also works:

```bash
ollama pull nomic-embed-text
```

Use these UI values:

```text
Base URL:  http://127.0.0.1:11434/v1
API key:   ollama
Model:     nomic-embed-text
Dimensions: 768
```

InMemory avoids PostgreSQL, but it still requires an embedder because it runs
the actual `knowledge.Load` and vector search path.

### Docling

Docling is optional and only needed for PDF, Office, image, or other binary
document extraction:

```bash
docker compose --profile docling up -d docling
```

The UI defaults to `http://127.0.0.1:5001`. OCR, Markdown/Text output, and image
reference mode can be changed before each preview.

## What is implemented

- Embedded single-binary web UI with a loopback-only default listener.
- Markdown, JSON, CSV, text, and Proto readers.
- Fixed, Recursive, Markdown, JSON, and reader-default chunking.
- Unicode-rune budget validation, actual-overlap calculation, source mapping,
  and over-budget visibility.
- Four edge-case samples covering tables, code blocks, mixed scripts, numeric
  JSON keys, quoted CSV fields, empty fields, and long text.
- Direct and Docling extraction.
- PGVector HNSW indexing with optional TSVector and RRF/weighted hybrid fusion.
- InMemory indexing for a quick ephemeral path.
- Actual `knowledge.Load` and `knowledge.Search` execution.
- Request size, chunk count, concurrency, and HTTP timeout limits.

The preview is intentionally ephemeral and expires after 30 minutes. Index
sessions live until the process exits and are capped to keep local resource
usage bounded.

## Architecture

```text
Browser
  ├─ Pipeline Preview
  │    upload/text → Direct or Docling → Reader → Chunking
  │                                      └→ chunks + source trace + metadata
  └─ Retrieval Lab
       preview snapshot → Embedder → InMemory or PGVector → knowledge.Search

Go server
  └─ trpc-agent-go Knowledge components are the only execution path
```

The server never sends uploaded content to a service except the Docling and
embedding endpoints explicitly selected by the user. This prototype has no
authentication and is intended for local development, so external listening
must be enabled deliberately.

## Validation

```bash
go test ./...
go vet ./...
go build ./...
```
