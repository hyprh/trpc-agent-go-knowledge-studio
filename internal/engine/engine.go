// Package engine owns the running knowledge bases and the task queue.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/knowledge"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/embedder"
	openaiembedder "trpc.group/trpc-go/trpc-agent-go/knowledge/embedder/openai"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/source"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore"
	"trpc.group/trpc-go/trpc-agent-go/knowledge/vectorstore/inmemory"

	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/model"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/sourcefactory"
	"github.com/hyprh/trpc-agent-go-knowledge-workbench/internal/store"
)

// ErrEmbedderNotConfigured is returned when an operation needs embeddings but
// the base has no embedding model set.
var ErrEmbedderNotConfigured = errors.New(
	"this knowledge base has no embedding model configured — set one under Embedding before ingesting")

// ErrStoreUnsupported is returned for vector backends that are not wired yet.
type ErrStoreUnsupported struct{ Type string }

func (e ErrStoreUnsupported) Error() string {
	return fmt.Sprintf("vector store %q is not wired up in this build; only \"inmemory\" is available", e.Type)
}

// instance is a live knowledge base plus the objects it was built from.
type instance struct {
	kb    *knowledge.BuiltinKnowledge
	store vectorstore.VectorStore
	emb   embedder.Embedder

	// stamp identifies the configuration this instance was built from. When it
	// no longer matches the stored base, the instance is rebuilt.
	stamp string
	// registered tracks which source IDs upstream believes it owns.
	registered map[string]bool

	// users counts callers currently holding this instance, and retired marks
	// it as superseded. An instance is closed only once both are settled: a
	// search or a sync task can be part way through using one when a config
	// edit replaces it, and closing it underneath them tears down the store
	// they are reading.
	users   int
	retired bool
}

// Engine owns knowledge-base instances and runs tasks against them.
type Engine struct {
	st  *store.Store
	hub *Hub

	mu   sync.Mutex
	inst map[string]*instance
	// building serialises construction per base, so two cold requests do not
	// each build an instance and leave one of them orphaned.
	building map[string]*sync.Mutex

	qmu    sync.Mutex
	queues map[string]chan string // baseID -> task IDs

	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc // taskID -> cancel

	wg   sync.WaitGroup
	done chan struct{}
}

// New creates an engine.
func New(st *store.Store, hub *Hub) *Engine {
	return &Engine{
		st:       st,
		hub:      hub,
		inst:     map[string]*instance{},
		building: map[string]*sync.Mutex{},
		queues:   map[string]chan string{},
		cancels:  map[string]context.CancelFunc{},
		done:     make(chan struct{}),
	}
}

// Close stops all workers and releases instances.
func (e *Engine) Close() {
	close(e.done)
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, in := range e.inst {
		e.retireLocked(in)
		delete(e.inst, id)
	}
}

// Invalidate drops the cached instance of a base, forcing a rebuild on next
// use. Call after editing storage or embedding configuration.
func (e *Engine) Invalidate(baseID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in, ok := e.inst[baseID]; ok {
		delete(e.inst, baseID)
		// Retire rather than close: a search or a running task may still hold
		// it, and closing the store out from under them fails the operation
		// for a reason the caller cannot act on.
		e.retireLocked(in)
	}
}

// stampOf renders the configuration fingerprint an instance depends on.
func stampOf(b *model.Base) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d|%t",
		b.VectorStore.Type, b.Embedder.Provider, b.Embedder.BaseURL, b.Embedder.Model,
		b.Embedder.Dimensions, b.IndexRevision, b.EnableSourceSync)
}

// acquire returns a live instance for a base, building it if needed.
//
// Sources that have already been ingested are registered at construction time:
// upstream keeps its source list in memory, and ReloadSource/RemoveSource look
// a source up by name, so after a restart they would fail without this.
func (e *Engine) acquire(ctx context.Context, baseID string) (*instance, func(), error) {
	return e.acquireFor(ctx, baseID, true)
}

// acquireFor builds an instance, optionally without insisting on an embedder.
//
// Deleting a source only calls DeleteByFilter on the vector store, so refusing
// the operation because no embedding model is set would strand a source that
// can never be ingested and can never be removed.
// The returned release function must be called when the caller is done with
// the instance; the instance is not closed until every holder has released it.
func (e *Engine) acquireFor(ctx context.Context, baseID string, requireEmbedder bool) (*instance, func(), error) {
	b, err := e.st.GetBase(ctx, baseID)
	if err != nil {
		return nil, nil, err
	}
	stamp := stampOf(b)

	if in, ok := e.checkout(baseID, stamp); ok {
		return in, func() { e.release(in) }, nil
	}

	// Build under a per-base lock. Without it two cold requests each construct
	// an instance, the second replaces the first, and the first is closed while
	// its owner is still using it.
	bl := e.buildLock(baseID)
	bl.Lock()
	defer bl.Unlock()

	// Another builder may have finished while this one waited.
	if in, ok := e.checkout(baseID, stamp); ok {
		return in, func() { e.release(in) }, nil
	}

	if requireEmbedder && !b.Embedder.Configured() {
		return nil, nil, ErrEmbedderNotConfigured
	}
	vs, err := buildVectorStore(b)
	if err != nil {
		return nil, nil, err
	}
	emb := buildEmbedder(b)

	srcs, err := e.st.ListSources(ctx, baseID)
	if err != nil {
		return nil, nil, err
	}
	registered := map[string]bool{}
	var upstream []source.Source
	for _, s := range srcs {
		if s.NeverSynced() || s.State == model.SourceDisabled {
			continue
		}
		obj, err := sourcefactory.Build(s)
		if err != nil {
			// A source whose config no longer validates must not take down the
			// whole base; it simply is not registered and will report on sync.
			continue
		}
		upstream = append(upstream, obj)
		registered[s.ID] = true
	}

	kb := knowledge.New(
		knowledge.WithVectorStore(vs),
		knowledge.WithEmbedder(emb),
		knowledge.WithEnableSourceSync(b.EnableSourceSync),
		knowledge.WithSources(upstream),
	)
	in := &instance{kb: kb, store: vs, emb: emb, stamp: stamp, registered: registered}

	e.mu.Lock()
	if old, ok := e.inst[baseID]; ok {
		e.retireLocked(old)
	}
	in.users = 1 // handed to this caller
	e.inst[baseID] = in
	e.mu.Unlock()
	return in, func() { e.release(in) }, nil
}

// checkout hands out the cached instance when its fingerprint still matches,
// counting the caller as a user.
func (e *Engine) checkout(baseID, stamp string) (*instance, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in, ok := e.inst[baseID]
	if !ok || in.stamp != stamp || in.retired {
		return nil, false
	}
	in.users++
	return in, true
}

func (e *Engine) release(in *instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in.users--
	e.closeIfIdleLocked(in)
}

// retireLocked marks an instance superseded and closes it if nobody holds it.
func (e *Engine) retireLocked(in *instance) {
	in.retired = true
	e.closeIfIdleLocked(in)
}

func (e *Engine) closeIfIdleLocked(in *instance) {
	if in.retired && in.users <= 0 && in.kb != nil {
		_ = in.kb.Close()
		in.kb = nil
	}
}

func (e *Engine) buildLock(baseID string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	bl, ok := e.building[baseID]
	if !ok {
		bl = &sync.Mutex{}
		e.building[baseID] = bl
	}
	return bl
}

func buildVectorStore(b *model.Base) (vectorstore.VectorStore, error) {
	switch b.VectorStore.Type {
	case "", "inmemory":
		return inmemory.New(), nil
	default:
		return nil, ErrStoreUnsupported{Type: b.VectorStore.Type}
	}
}

func buildEmbedder(b *model.Base) embedder.Embedder {
	opts := []openaiembedder.Option{openaiembedder.WithModel(b.Embedder.Model)}
	if b.Embedder.BaseURL != "" {
		opts = append(opts, openaiembedder.WithBaseURL(b.Embedder.BaseURL))
	}
	if b.Embedder.APIKey != "" {
		opts = append(opts, openaiembedder.WithAPIKey(b.Embedder.APIKey))
	}
	if b.Embedder.Dimensions > 0 {
		opts = append(opts, openaiembedder.WithDimensions(b.Embedder.Dimensions))
	}
	return openaiembedder.New(opts...)
}

// ---------------------------------------------------------------- queue

// Enqueue persists a task and schedules it.
func (e *Engine) Enqueue(ctx context.Context, t *model.Task) error {
	if err := e.st.CreateTask(ctx, t); err != nil {
		return err
	}
	e.hub.Publish(Event{Type: "task", BaseID: t.BaseID, Task: t})
	e.schedule(t.BaseID, t.ID)
	return nil
}

// schedule pushes a task ID onto its base's queue, starting the worker if this
// is the first task for that base.
//
// One worker per base, and a worker runs exactly one task at a time: upstream
// serializes every mutating call on a per-knowledge-base mutex, so any wider
// concurrency here would only queue up inside the lock.
func (e *Engine) schedule(baseID, taskID string) {
	e.qmu.Lock()
	q, ok := e.queues[baseID]
	if !ok {
		q = make(chan string, 256)
		e.queues[baseID] = q
		e.wg.Add(1)
		go e.worker(baseID, q)
	}
	e.qmu.Unlock()

	select {
	case q <- taskID:
	default:
		// Queue full: fail fast rather than block the HTTP handler.
		_ = e.st.FinishTask(context.Background(), taskID, model.TaskFailed,
			model.Counters{}, "task queue is full for this knowledge base")
	}
}

func (e *Engine) worker(baseID string, q chan string) {
	defer e.wg.Done()
	for {
		select {
		case <-e.done:
			return
		case taskID := <-q:
			e.run(baseID, taskID)
		}
	}
}

// Cancel requests cancellation of a running task.
func (e *Engine) Cancel(taskID string) bool {
	e.cancelMu.Lock()
	cancel, ok := e.cancels[taskID]
	e.cancelMu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// Resume re-arms queued and interrupted tasks after a restart, and drops any
// ingest state that could not have survived the process.
func (e *Engine) Resume(ctx context.Context) error {
	bases, err := e.st.ListBases(ctx)
	if err != nil {
		return err
	}
	for _, b := range bases {
		if b.VectorStore.Persistent() {
			continue
		}
		n, err := e.st.ResetSyncState(ctx, b.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			log.Printf("base %q uses a non-persistent store: reset %d source(s) to never-synced",
				b.Name, n)
		}
	}

	pending, err := e.st.PendingTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range pending {
		if t.State == model.TaskRunning {
			// The process died mid-task. The unique index allows only one
			// running row per base, so it must be cleared before rescheduling.
			_ = e.st.FinishTask(ctx, t.ID, model.TaskFailed, model.Counters{},
				"interrupted by a workbench restart")
			continue
		}
		e.schedule(t.BaseID, t.ID)
	}
	return nil
}

// run executes one task to completion.
func (e *Engine) run(baseID, taskID string) {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancelMu.Lock()
	e.cancels[taskID] = cancel
	e.cancelMu.Unlock()
	defer func() {
		cancel()
		e.cancelMu.Lock()
		delete(e.cancels, taskID)
		e.cancelMu.Unlock()
	}()

	t, err := e.st.GetTask(ctx, taskID)
	if err != nil {
		return
	}
	if t.Terminal() {
		return
	}

	before := e.counters(ctx, baseID, t.SourceID)
	if err := e.st.StartTask(ctx, taskID, before); err != nil {
		return
	}
	// A new attempt supersedes the previous failure. Without this the source
	// keeps showing the old message after the user has fixed the cause, which
	// reads as if the fix did not take.
	if t.SourceID != "" {
		_ = e.st.MarkSourceFailed(ctx, t.SourceID, "")
	}
	t.State = model.TaskRunning
	now := time.Now()
	t.StartedAt = &now
	t.CountersBefore = before
	e.hub.Publish(Event{Type: "task", BaseID: baseID, Task: t})

	execErr := e.execute(ctx, t)

	after := e.counters(ctx, baseID, t.SourceID)
	after.Docs = t.Progress.Total
	state := model.TaskSucceeded
	msg := ""
	switch {
	case execErr != nil && errors.Is(execErr, context.Canceled):
		state, msg = model.TaskCanceled, "canceled"
	case execErr != nil:
		state, msg = model.TaskFailed, execErr.Error()
	case t.Kind != model.TaskRemoveSource && after.Chunks == 0:
		// A synced source always owns at least one chunk. Counting by the
		// absolute total rather than the delta matters: re-syncing unchanged
		// content legitimately leaves the count where it was.
		state = model.TaskFailed
		msg = zeroChunkReason(after.Docs, sourceTypeOf(ctx, e.st, t.SourceID))
	}
	_ = e.st.FinishTask(ctx, taskID, state, after, msg)

	if t.SourceID != "" {
		if state == model.TaskSucceeded && t.Kind != model.TaskRemoveSource {
			// Record the hash this task actually ingested, not whatever the row
			// says now. An edit made while the sync was running belongs to the
			// next sync; marking it applied here would report configuration
			// that never ran as live.
			_ = e.st.MarkSourceSynced(ctx, t.SourceID, t.ConfigHash, after)
		} else if state == model.TaskFailed {
			_ = e.st.MarkSourceFailed(ctx, t.SourceID, msg)
		}
	}

	final, err := e.st.GetTask(context.Background(), taskID)
	if err == nil {
		e.hub.Publish(Event{Type: "task", BaseID: baseID, Task: final})
	}
	e.hub.Publish(Event{Type: "sources_changed", BaseID: baseID})
}

func (e *Engine) execute(ctx context.Context, t *model.Task) error {
	// A source that was never ingested has no vectors anywhere, so removing it
	// is a pure control-plane delete. Handle it before touching the engine:
	// building an instance can fail for reasons that have nothing to do with
	// deletion, and the user would then be unable to remove the row at all.
	if t.Kind == model.TaskRemoveSource {
		src, err := e.st.GetSource(ctx, t.SourceID)
		if err != nil {
			return err
		}
		if src.NeverSynced() {
			return e.st.DeleteSource(ctx, src.ID)
		}
	}

	in, release, err := e.acquireFor(ctx, t.BaseID, t.Kind != model.TaskRemoveSource)
	if err != nil {
		return err
	}
	defer release()

	progress := knowledge.WithLoadProgressCallback(func(_ context.Context, ev knowledge.LoadProgressEvent) {
		if ev.Total > t.Progress.Total {
			t.Progress.Total = ev.Total
		}
		p := model.Progress{
			SourceName:      ev.SourceName,
			SourceProcessed: ev.SourceProcessed,
			SourceTotal:     ev.SourceTotal,
			SourceElapsedMS: ev.SourceElapsed.Milliseconds(),
			SourceETAMS:     ev.SourceETA.Milliseconds(),
			Total:           ev.Total,
			TotalElapsedMS:  ev.TotalElapsed.Milliseconds(),
			Done:            ev.Done,
		}
		_ = e.st.SaveProgress(context.Background(), t.ID, p)
		e.hub.Publish(Event{Type: "progress", BaseID: t.BaseID, TaskID: t.ID, Progress: &p})
	})

	switch t.Kind {
	case model.TaskLoadAll:
		return in.kb.Load(ctx, progress)

	case model.TaskRemoveSource:
		src, err := e.st.GetSource(ctx, t.SourceID)
		if err != nil {
			return err
		}
		// A source that was never ingested has no vectors and was never
		// registered upstream; RemoveSource would fail with "not found".
		if src.NeverSynced() || !in.registered[src.ID] {
			return e.st.DeleteSource(ctx, src.ID)
		}
		if err := in.kb.RemoveSource(ctx, src.ID); err != nil {
			return err
		}
		e.setRegistered(t.BaseID, src.ID, false)
		return e.st.DeleteSource(ctx, src.ID)

	case model.TaskAddSource, model.TaskReloadSource:
		src, err := e.st.GetSource(ctx, t.SourceID)
		if err != nil {
			return err
		}
		// Build from the snapshot taken when the task was queued, so a config
		// edit that lands mid-queue does not change what this run ingests.
		snapshot := *src
		snapshot.Config = t.Params
		if t.Metadata != nil {
			snapshot.UserMetadata = t.Metadata
		}
		obj, err := sourcefactory.Build(&snapshot)
		if err != nil {
			return err
		}
		// Which upstream call applies depends on whether upstream already
		// knows this name, not on how the task was labelled: a task queued as
		// "add" can find the source registered if an earlier sync succeeded.
		if in.registered[src.ID] {
			if err := in.kb.ReloadSource(ctx, obj, progress); err != nil {
				return err
			}
		} else {
			if err := in.kb.AddSource(ctx, obj, progress); err != nil {
				return err
			}
			e.setRegistered(t.BaseID, src.ID, true)
		}
		return nil

	default:
		return fmt.Errorf("unknown task kind %q", t.Kind)
	}
}

// sourceTypeOf looks up a source's type for error reporting, tolerating a row
// that has already been deleted.
func sourceTypeOf(ctx context.Context, st *store.Store, id string) model.SourceType {
	if id == "" {
		return ""
	}
	src, err := st.GetSource(ctx, id)
	if err != nil {
		return ""
	}
	return src.Type
}

// zeroChunkReason explains a sync that upstream reported as successful but
// that left no vectors attributable to the source.
//
// Upstream returns nil in every case below, so without this the UI would show
// a green "synced" for a source that ingested nothing.
func zeroChunkReason(docs int, t model.SourceType) string {
	if docs == 0 {
		return "读到 0 个文档，没有任何内容入库。检查 reader 类型是否适用于这些文件、" +
			"扩展名过滤是否过严、路径是否存在。"
	}
	// The auto source is the one type known to mislabel what it produces, so
	// name it only when it is actually the type in play — blaming it for a dir
	// source sends the reader off in the wrong direction entirely.
	if t == model.SourceAuto {
		return fmt.Sprintf(
			"读到了 %d 个文档，但没有一条向量记在这个 source 名下。"+
				"auto 类型会把工作委派给内部的 dir/file source，而委派时没有传下 source 名"+
				"（upstream knowledge/source/auto/auto_source.go 构造子 source 时缺少 WithName），"+
				"文档因此被标成 \"Directory Source\" 之类的默认名。"+
				"后果是删除这个 source 时按名字过滤也删不到向量，留下孤儿数据。"+
				"改用 dir 或 file 类型可以绕开。", docs)
	}
	return fmt.Sprintf(
		"读到了 %d 个文档，但没有一条向量记在这个 source 名下。"+
			"常见原因是向量库里已有的记录与本次写入对不上，或者写入实际没有落库。"+
			"再同步一次通常可以恢复；若持续如此，检查 embedding 服务与向量库连接。", docs)
}

func (e *Engine) setRegistered(baseID, sourceID string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in, ok := e.inst[baseID]; ok {
		in.registered[sourceID] = on
		if !on {
			delete(in.registered, sourceID)
		}
	}
}

// counters samples the vector store. Load progress events report document
// counts only, so chunk deltas have to be measured before and after.
func (e *Engine) counters(ctx context.Context, baseID, sourceID string) model.Counters {
	e.mu.Lock()
	in, ok := e.inst[baseID]
	if ok && !in.retired && in.kb != nil {
		in.users++
	} else {
		ok = false
	}
	e.mu.Unlock()
	if !ok {
		return model.Counters{}
	}
	defer e.release(in)
	var opts []vectorstore.CountOption
	if sourceID != "" {
		opts = append(opts, vectorstore.WithCountFilter(map[string]any{
			source.MetaSourceName: sourceID,
		}))
	}
	n, err := in.store.Count(ctx, opts...)
	if err != nil {
		return model.Counters{}
	}
	return model.Counters{Chunks: n}
}

// Search runs a retrieval query against a base.
func (e *Engine) Search(ctx context.Context, baseID string, req *knowledge.SearchRequest) (*knowledge.SearchResult, error) {
	in, release, err := e.acquire(ctx, baseID)
	if err != nil {
		return nil, err
	}
	defer release()
	return in.kb.Search(ctx, req)
}
