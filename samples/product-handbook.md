# Knowledge Pipeline Handbook

This sample demonstrates how a production knowledge pipeline behaves across headings, paragraphs, tables, code blocks, lists, and multilingual punctuation. It is intentionally long enough to expose small tails and awkward boundaries when the chunk size changes.

## From source to retrieval

A useful knowledge system is not defined by the upload button. The important path starts with a source, converts complex content into a readable representation, creates context-preserving chunks, embeds those chunks, writes them to a searchable store, and evaluates whether retrieval returns evidence that an agent can actually use. Every stage should be visible because a high-quality answer can still hide a fragile ingestion pipeline.

### Pipeline stages

1. Source identifies the input and attaches stable provenance.
2. Extractor converts complex binary documents into Markdown or text.
3. Reader understands the content format.
4. Chunking preserves useful semantic boundaries.
5. Embedder creates the vector representation.
6. VectorStore supports vector, keyword, hybrid, and filtered retrieval.
7. Reranker determines the final order presented to the agent.

## Configuration matrix

| Content | Preferred extractor | Reader | Chunking | Notes |
| --- | --- | --- | --- | --- |
| Markdown documentation | Direct | Markdown | Markdown | Preserve the full heading path |
| Complex PDF | Docling | Markdown | Markdown | Inspect tables, images, and OCR output |
| JSON catalog | Direct | JSON | JSON | Keep nested keys and deterministic ordering |
| CSV export | Direct | CSV | Fixed | Preserve complete records whenever possible |
| Source repository | Direct | Language AST | Structural | Retain file and line metadata |

The table should remain understandable even when it spans multiple chunks. Repeating a compact header may improve retrieval, but blindly copying a very wide table header can also consume the entire token budget.

## Retrieval experiment

The retrieval lab should separate the stages below:

```text
user query
  -> enhanced query
  -> vector and keyword candidates
  -> metadata filters
  -> reranker
  -> agent context
```

The raw candidate score and the reranked score answer different questions. Hiding either one makes it difficult to determine whether a bad answer was caused by chunking, embedding, filtering, or ranking.

```go
kb := knowledge.New(
    knowledge.WithSources(sources),
    knowledge.WithEmbedder(embeddingModel),
    knowledge.WithVectorStore(store),
    knowledge.WithReranker(reranker),
)
if err := kb.Load(ctx); err != nil {
    return err
}
```

## Mixed characters and natural boundaries

English version numbers such as 12.6, 2.8.12, and v1.2.3 should not be treated as sentence boundaries. Abbreviations, URLs, and decimal numbers create similar problems. A natural splitter should prefer punctuation and whitespace without cutting through arbitrary words.

中文测试只用于覆盖字符边界：知识库需要正确处理中文标点？！连续标点不应制造空块；一个中文字符是一个 Unicode rune，但经过不同 tokenizer 后可能对应不同数量的 token。

Emoji also matters: 👩‍💻, 🧭, and family sequences may consist of multiple code points even when the user perceives each sequence as one character.

## Long paragraph

A long paragraph exists here to force recursive splitting. The implementation should search for paragraph, sentence, punctuation, and whitespace boundaries before it falls back to a hard rune boundary. It should also rebalance a very small tail when doing so does not violate the configured budget. The purpose of this paragraph is not literary quality; it is to make the boundary behavior observable and repeatable across several chunk sizes, especially values around one hundred and two hundred runes where headings and their first paragraphs frequently compete for the same budget.

## Header without a body
