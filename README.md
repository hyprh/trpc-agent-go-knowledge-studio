# Knowledge Workbench

A control plane for `trpc-agent-go` knowledge bases: define sources, inspect how
they chunk, ingest them, and search the result.

Upstream `BuiltinKnowledge` keeps its sources in memory and exposes only
`Name`/`Type`/`GetMetadata` on a source. This project supplies the missing
layer: a database that owns source configuration, a task queue that respects
upstream's per-base write lock, and a UI over both.

## Run

```bash
go run . -addr 127.0.0.1:8090 -db workbench.db
```

Open <http://127.0.0.1:8090>. The server binds to loopback unless `-addr` says
otherwise.

The chunk inspector needs nothing configured — it runs the real chunking
strategies over a file and reports boundaries, sizes, and budget overruns.
Ingesting and searching additionally need an embedding endpoint.

### Embeddings

Any OpenAI-compatible endpoint works. For a local Ollama:

```bash
ollama pull nomic-embed-text
```

Then, under **Overview & settings → Embedding**:

```text
Model:      nomic-embed-text
Dimensions: 768
Base URL:   http://127.0.0.1:11434/v1
API key:    ollama
```

## What is implemented

| Area | State |
|---|---|
| Knowledge base CRUD, settings | done |
| Source CRUD for file / dir / url / auto / repo | done |
| Task queue with live progress over SSE, cancel | done |
| Chunk inspector (file tree, live re-chunking, budget metrics) | done |
| Retrieval | basic — vector search, single score |
| Vector stores | `inmemory` only; others return an explicit 501 |
| Graph explorer | not yet (v1) |
| Agent console | not yet (v2) |

## Design notes

Four decisions that are not obvious from the code, and would be easy to
"simplify" into bugs.

**A source's id is its upstream name, and it never changes.** Upstream deletes
a source's vectors by filtering on `trpc_agent_go_source_name`. If that name
tracked a user-editable display name, renaming a source would orphan every
vector already written. `display_name` is separate and free to change.

**Editing a source's configuration does not touch the running knowledge base.**
The upstream `Source` interface cannot be asked what it was built with, so the
database is the only authority and reconciliation is impossible. An edit lands
in the database and the row becomes *stale* — `config_hash != synced_config_hash`
— until a sync task runs. That comparison is also how the UI decides what to
show; there is no separate "dirty" flag that could disagree.

**`repo` sources have no chunking parameters.** They perform AST extraction, not
text chunking, and upstream `reposource` accepts no chunk size or overlap. The
config is a discriminated union so that sending `chunk_size` on a repo source is
rejected at the boundary rather than silently ignored.

**Writes serialize per knowledge base.** Every mutating upstream call takes
`dataOperationMu`, so the queue runs one write at a time per base; a partial
unique index on `task(base_id) WHERE state = 'running'` enforces the same
invariant in the database. `Sync all` enqueues one task per source rather than
a single `Load`, which costs nothing given the lock and buys per-source
progress and cancellation.

## Layout

```
main.go                     flags, wiring, graceful shutdown
internal/model/             the data model and its validation rules
internal/store/             SQLite schema and queries
internal/sourcefactory/     the only code that knows upstream's option funcs
internal/engine/            knowledge-base instances, task queue, event hub
internal/preview/           stateless chunking preview and source file trees
internal/api/               HTTP routes
web/                        UI
```

## Upstream version

This builds against [trpc-agent-go#2215](https://github.com/trpc-group/trpc-agent-go/pull/2215)
("knowledge: improve chunking boundaries and examples") rather than a released
tag, through `replace` directives in `go.mod` that point at a sibling checkout:

```
replace trpc.group/trpc-go/trpc-agent-go => ../trpc-agent-go-pr2215
```

Three reader packages are separate modules and need their own directives, which
`go.mod` already carries. To set the checkout up, from a clone of
`trpc-group/trpc-agent-go` next to this repository:

```bash
git fetch origin pull/2215/head:pr-2215
git worktree add ../trpc-agent-go-pr2215 pr-2215
```

The result should sit beside this repository:

```
trpc-agent-go-workbench/     this repository
trpc-agent-go-pr2215/        the PR checkout the replace directives resolve to
```

Drop every `replace` once the PR merges and pin a released version instead.

### Two upstream issues this surfaced

**Readers must be imported to exist.** Every reader registers itself from
`init()`, and `GetFileType(".go")` consults that registry — with the Go reader
absent it silently downgrades to the text reader, and an explicit
`reader_type: "go"` fails per file with `no reader available for file type: go`
while `Load` still returns nil. A directory of 82 Go files ingested as zero
documents and reported success. `internal/readers` exists solely to import them
all; `main` logs the registered extensions at startup so the failure can never
be silent again.

**Auto sources mislabel their documents.** `knowledge/source/auto/auto_source.go`
delegates to an internal dir/file/url source but does not pass its own name
down (`dirsource.New([]string{input}, opts...)` with no `WithName`), so every
document it produces is stamped
`trpc_agent_go_source_name = "Directory Source"` instead of the source's name.
Since `RemoveSource` deletes by exactly that filter, **deleting an auto source
removes none of its vectors** and orphans all of them. The same directory
ingested as a `dir` source behaves correctly. Until upstream passes the name
through, prefer `dir`/`file` over `auto`; the workbench fails the sync with an
explanation rather than showing a green "synced" for a source whose vectors
cannot be attributed to it.

### Why that branch

On `main`, chunking emits chunks larger than the configured budget — overlap is
appended on top of a full-size core, so a chunk can reach `chunkSize + overlap`
— and it over-splits badly. Measured on this repo's own `internal/model/model.go`
at `recursive 800/100`:

| | chunks | avg | max |
|---|---|---|---|
| main | 122 | 177 | 868 (over budget by 68) |
| #2215 | 26 | — | 795 (within budget) |

The inspector's `over budget` metric counts these either way; on #2215 it reads
zero across every size/overlap combination tried.
