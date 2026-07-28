const state = {
  samples: [],
  selectedSample: null,
  currentFile: null,
  preview: null,
  index: null,
  metadataVisible: false,
  sourceTab: "extracted",
  activeChunk: -1,
  syncGuardUntil: 0,
};

const $ = (id) => document.getElementById(id);

const elements = {
  healthPill: $("healthPill"),
  healthLabel: $("healthLabel"),
  sampleSelect: $("sampleSelect"),
  fileInput: $("fileInput"),
  resetInputButton: $("resetInputButton"),
  documentName: $("documentName"),
  inputState: $("inputState"),
  extractorSelect: $("extractorSelect"),
  doclingFields: $("doclingFields"),
  doclingEndpoint: $("doclingEndpoint"),
  outputFormat: $("outputFormat"),
  imageMode: $("imageMode"),
  ocrEnabled: $("ocrEnabled"),
  readerSelect: $("readerSelect"),
  strategySelect: $("strategySelect"),
  chunkSize: $("chunkSize"),
  overlap: $("overlap"),
  chunkingNote: $("chunkingNote"),
  previewButton: $("previewButton"),
  copyConfigButton: $("copyConfigButton"),
  previewStatus: $("previewStatus"),
  metricInput: $("metricInput"),
  metricInputDetail: $("metricInputDetail"),
  metricChunks: $("metricChunks"),
  metricStrategy: $("metricStrategy"),
  metricAverage: $("metricAverage"),
  metricRange: $("metricRange"),
  metricBudget: $("metricBudget"),
  metricMapped: $("metricMapped"),
  syncScroll: $("syncScroll"),
  metadataButton: $("metadataButton"),
  warningBanner: $("warningBanner"),
  sourceTitle: $("sourceTitle"),
  sourceEditor: $("sourceEditor"),
  sourceMap: $("sourceMap"),
  sourceFooter: $("sourceFooter"),
  extractedTab: $("extractedTab"),
  rawTab: $("rawTab"),
  chunksTitle: $("chunksTitle"),
  chunksList: $("chunksList"),
  chunksFooter: $("chunksFooter"),
  sizeUnitChip: $("sizeUnitChip"),
  connectorLayer: $("connectorLayer"),
  openRetrievalButton: $("openRetrievalButton"),
  previewContext: $("previewContext"),
  storeType: $("storeType"),
  pgvectorFields: $("pgvectorFields"),
  pgvectorDSN: $("pgvectorDSN"),
  pgvectorTable: $("pgvectorTable"),
  fusionMode: $("fusionMode"),
  enableTextSearch: $("enableTextSearch"),
  recreateIndex: $("recreateIndex"),
  embedderBaseURL: $("embedderBaseURL"),
  embedderAPIKey: $("embedderAPIKey"),
  embedderModel: $("embedderModel"),
  embedderDimensions: $("embedderDimensions"),
  rerankerTopK: $("rerankerTopK"),
  buildIndexButton: $("buildIndexButton"),
  indexStatus: $("indexStatus"),
  indexBadge: $("indexBadge"),
  searchQuery: $("searchQuery"),
  searchMode: $("searchMode"),
  maxResults: $("maxResults"),
  minScore: $("minScore"),
  metadataFilter: $("metadataFilter"),
  searchButton: $("searchButton"),
  searchStatus: $("searchStatus"),
  searchResults: $("searchResults"),
  toastRegion: $("toastRegion"),
  pipelineView: $("pipelineView"),
  retrievalView: $("retrievalView"),
};

function setBusy(button, busy, label) {
  if (!button.dataset.defaultLabel) {
    button.dataset.defaultLabel = button.querySelector("span")?.textContent || button.textContent;
  }
  button.disabled = busy;
  const target = button.querySelector("span") || button;
  target.textContent = busy ? label : button.dataset.defaultLabel;
}

function showToast(message, isError = false) {
  const toast = document.createElement("div");
  toast.className = `toast${isError ? " error" : ""}`;
  toast.textContent = message;
  elements.toastRegion.appendChild(toast);
  window.setTimeout(() => toast.remove(), 4200);
}

async function api(path, options = {}) {
  const response = await fetch(path, options);
  const type = response.headers.get("content-type") || "";
  const payload = type.includes("application/json") ? await response.json() : null;
  if (!response.ok) {
    throw new Error(payload?.error || `${response.status} ${response.statusText}`);
  }
  return payload;
}

function switchView(name) {
  const pipeline = name === "pipeline";
  elements.pipelineView.classList.toggle("active", pipeline);
  elements.retrievalView.classList.toggle("active", !pipeline);
  document.querySelectorAll(".nav-button").forEach((button) => {
    button.classList.toggle("active", button.dataset.view === name);
  });
  if (!pipeline) {
    updatePreviewContext();
  }
  window.setTimeout(drawConnectors, 50);
}

async function checkHealth() {
  try {
    await api("/api/health");
    elements.healthPill.classList.add("online");
    elements.healthPill.classList.remove("offline");
    elements.healthLabel.textContent = "Backend ready";
  } catch (error) {
    elements.healthPill.classList.add("offline");
    elements.healthPill.classList.remove("online");
    elements.healthLabel.textContent = "Backend unavailable";
  }
}

async function loadSamples() {
  state.samples = await api("/api/samples");
  elements.sampleSelect.replaceChildren();
  state.samples.forEach((sample) => {
    const option = document.createElement("option");
    option.value = sample.id;
    option.textContent = `${sample.name} · ${sample.description}`;
    elements.sampleSelect.appendChild(option);
  });
  if (state.samples.length > 0) {
    loadSample(state.samples[0].id);
  }
}

function loadSample(id) {
  const sample = state.samples.find((item) => item.id === id);
  if (!sample) {
    return;
  }
  state.selectedSample = sample;
  state.currentFile = null;
  elements.fileInput.value = "";
  elements.documentName.value = sample.name;
  elements.sourceEditor.value = sample.content;
  elements.sourceEditor.readOnly = false;
  elements.sourceEditor.hidden = false;
  elements.sourceMap.hidden = true;
  elements.sourceTitle.textContent = sample.name;
  elements.sourceFooter.textContent = sample.description;
  elements.inputState.textContent = "Sample";
  invalidatePreview();
}

function invalidatePreview() {
  state.preview = null;
  state.index = null;
  state.activeChunk = -1;
  elements.copyConfigButton.disabled = true;
  elements.metadataButton.disabled = true;
  elements.openRetrievalButton.disabled = true;
  elements.buildIndexButton.disabled = true;
  elements.searchButton.disabled = true;
  elements.warningBanner.hidden = true;
  elements.previewStatus.textContent = "Pipeline changed. Run preview to refresh chunks.";
  elements.previewStatus.classList.remove("error");
}

function isProbablyText(file) {
  const extension = file.name.split(".").pop()?.toLowerCase();
  return file.type.startsWith("text/") ||
    ["md", "markdown", "txt", "json", "csv", "proto", "yaml", "yml", "xml", "html"].includes(extension);
}

async function handleUpload() {
  const [file] = elements.fileInput.files;
  if (!file) {
    return;
  }
  state.currentFile = file;
  state.selectedSample = null;
  elements.documentName.value = file.name;
  elements.sourceTitle.textContent = file.name;
  elements.inputState.textContent = "Upload";
  if (isProbablyText(file)) {
    elements.sourceEditor.value = await file.text();
    elements.sourceEditor.readOnly = false;
    elements.sourceFooter.textContent = `${file.type || "text"} · ${formatBytes(file.size)} · editable before preview`;
  } else {
    elements.sourceEditor.value = "Binary document selected. Choose Docling and run Preview to inspect extracted content.";
    elements.sourceEditor.readOnly = true;
    elements.sourceFooter.textContent = `${file.type || "binary"} · ${formatBytes(file.size)}`;
    elements.extractorSelect.value = "docling";
    updateExtractorFields();
  }
  elements.sourceEditor.hidden = false;
  elements.sourceMap.hidden = true;
  invalidatePreview();
}

function updateExtractorFields() {
  elements.doclingFields.hidden = elements.extractorSelect.value !== "docling";
}

function updateStrategyFields() {
  const json = elements.strategySelect.value === "json";
  if (json) {
    elements.overlap.value = "0";
  }
  elements.overlap.disabled = json;
  const notes = {
    reader: "Reader default resolves from the selected document type.",
    fixed: "Fixed preserves predictable rune budgets and optional overlap.",
    recursive: "Recursive prefers paragraph, line, word, then rune boundaries.",
    markdown: "Markdown preserves structural context and header paths.",
    json: "JSON uses structural nodes; overlap is not applicable.",
  };
  elements.chunkingNote.textContent = notes[elements.strategySelect.value];
}

function previewConfig() {
  return {
    extractor: elements.extractorSelect.value,
    docling_endpoint: elements.doclingEndpoint.value.trim(),
    output_format: elements.outputFormat.value,
    ocr_enabled: elements.ocrEnabled.checked,
    image_mode: elements.imageMode.value,
    reader: elements.readerSelect.value,
    strategy: elements.strategySelect.value,
    chunk_size: Number(elements.chunkSize.value),
    overlap: Number(elements.overlap.value),
  };
}

async function runPreview() {
  const config = previewConfig();
  if (!Number.isInteger(config.chunk_size) || config.chunk_size < 16) {
    showToast("Chunk size must be an integer of at least 16 runes.", true);
    return;
  }
  if (!Number.isInteger(config.overlap) || config.overlap < 0 || config.overlap >= config.chunk_size) {
    showToast("Overlap must be non-negative and smaller than chunk size.", true);
    return;
  }

  const body = new FormData();
  body.set("config", JSON.stringify(config));
  if (state.currentFile) {
    body.set("file", state.currentFile, elements.documentName.value.trim() || state.currentFile.name);
  } else {
    body.set("name", elements.documentName.value.trim() || "document.md");
    body.set("content", elements.sourceEditor.value);
  }

  setBusy(elements.previewButton, true, "Running pipeline");
  elements.previewStatus.textContent = config.extractor === "docling"
    ? "Waiting for Docling, then reading and chunking…"
    : "Reading and chunking with trpc-agent-go…";
  elements.previewStatus.classList.remove("error");
  try {
    state.preview = await api("/api/preview", { method: "POST", body });
    state.index = null;
    state.sourceTab = "extracted";
    state.activeChunk = -1;
    renderPreview();
    showToast(`Created ${state.preview.summary.total} chunks.`);
  } catch (error) {
    elements.previewStatus.textContent = error.message;
    elements.previewStatus.classList.add("error");
    showToast(error.message, true);
  } finally {
    setBusy(elements.previewButton, false, "Run preview");
  }
}

function renderPreview() {
  const preview = state.preview;
  const summary = preview.summary;
  elements.metricInput.textContent = `${preview.extracted.runes.toLocaleString()} runes`;
  elements.metricInputDetail.textContent = `${formatBytes(preview.input.bytes)} input · ${preview.extracted.format}`;
  elements.metricChunks.textContent = summary.total.toLocaleString();
  elements.metricStrategy.textContent = `${summary.reader} · ${summary.strategy}`;
  elements.metricAverage.textContent = summary.average_size.toFixed(1);
  elements.metricRange.textContent = `${summary.min_size}–${summary.max_size} runes`;
  elements.metricBudget.textContent = summary.over_budget === 0 ? "OK" : `${summary.over_budget} over`;
  elements.metricMapped.textContent = `${summary.mapped_chunks}/${summary.total} mapped`;
  elements.sourceTitle.textContent = preview.input.name;
  const sourceAligned = summary.size_unit === "bytes" && summary.mapped_chunks > 1;
  elements.chunksTitle.textContent = `${summary.total} chunks · ${summary.strategy}${sourceAligned ? " · source aligned" : ""}`;
  elements.sizeUnitChip.textContent = summary.size_unit;
  elements.sourceFooter.textContent = preview.extracted.applied
    ? `${preview.input.content_type} → ${preview.extracted.format} via Docling`
    : `${preview.input.content_type} · direct reader input`;
  elements.chunksFooter.textContent = `${summary.mapped_chunks} source mappings · ${summary.over_budget} budget violations`;
  elements.previewStatus.textContent = `Preview ${shortID(preview.id)} · expires after 30 minutes.`;
  elements.previewStatus.classList.remove("error");
  elements.warningBanner.hidden = preview.warnings.length === 0 && summary.over_budget === 0;
  const warnings = [...preview.warnings];
  if (summary.over_budget > 0) {
    warnings.unshift(`${summary.over_budget} chunks exceed the configured ${preview.config.chunk_size}-rune budget.`);
  }
  elements.warningBanner.textContent = warnings.join(" ");
  elements.copyConfigButton.disabled = false;
  elements.metadataButton.disabled = false;
  elements.openRetrievalButton.disabled = false;
  elements.buildIndexButton.disabled = false;
  elements.searchButton.disabled = true;
  elements.indexStatus.textContent = "Ready to build an index from this exact preview snapshot.";
  renderSourceTab();
  renderChunks();
  updatePreviewContext();
  window.requestAnimationFrame(drawConnectors);
}

function renderSourceTab() {
  if (!state.preview) {
    return;
  }
  document.querySelectorAll("[data-source-tab]").forEach((button) => {
    button.classList.toggle("active", button.dataset.sourceTab === state.sourceTab);
  });
  if (state.sourceTab === "raw") {
    elements.sourceMap.hidden = true;
    elements.sourceEditor.hidden = false;
    elements.sourceEditor.readOnly = true;
    elements.sourceEditor.value = state.preview.input.text ||
      "Raw bytes are not valid display text. Switch to Extracted to inspect Docling output.";
    window.requestAnimationFrame(drawConnectors);
    return;
  }
  elements.sourceEditor.hidden = true;
  elements.sourceMap.hidden = false;
  buildSourceMap();
}

function buildSourceMap() {
  const content = state.preview.extracted.content;
  const runes = Array.from(content);
  const mapped = state.preview.chunks.filter((chunk) => chunk.mapped);
  const boundaries = new Set([0, runes.length]);
  const anchorsByOffset = new Map();
  mapped.forEach((chunk) => {
    boundaries.add(chunk.source_start);
    boundaries.add(chunk.source_end);
    if (chunk.overlap_start >= chunk.source_start && chunk.overlap_start <= chunk.source_end) {
      boundaries.add(chunk.overlap_start);
    }
    const anchors = anchorsByOffset.get(chunk.source_start) || [];
    anchors.push(chunk.index);
    anchorsByOffset.set(chunk.source_start, anchors);
  });
  const sorted = [...boundaries].filter((value) => value >= 0 && value <= runes.length).sort((a, b) => a - b);
  elements.sourceMap.replaceChildren();
  for (let i = 0; i < sorted.length - 1; i += 1) {
    const start = sorted[i];
    const end = sorted[i + 1];
    appendSourceAnchors(start, anchorsByOffset);
    if (end <= start) {
      continue;
    }
    const covering = mapped.filter((chunk) => chunk.source_start <= start && chunk.source_end >= end);
    const span = document.createElement("span");
    if (covering.length > 0) {
      span.className = "source-span";
      span.dataset.chunks = covering.map((chunk) => chunk.index).join(",");
      const overlap = covering.length > 1 ||
        covering.some((chunk) => chunk.actual_overlap > 0 && start < chunk.overlap_start);
      span.classList.toggle("overlap", overlap);
      span.addEventListener("click", () => activateChunk(covering[covering.length - 1].index, true));
    }
    span.textContent = runes.slice(start, end).join("");
    elements.sourceMap.appendChild(span);
  }
  appendSourceAnchors(runes.length, anchorsByOffset);
  updateActiveSource();
}

function appendSourceAnchors(offset, anchorsByOffset) {
  (anchorsByOffset.get(offset) || []).forEach((index) => {
    const anchor = document.createElement("span");
    anchor.id = `source-anchor-${index}`;
    anchor.className = "source-anchor";
    anchor.dataset.chunk = String(index);
    elements.sourceMap.appendChild(anchor);
  });
}

function renderChunks() {
  elements.chunksList.replaceChildren();
  elements.chunksList.classList.remove("empty");
  displayedChunks().forEach((chunk) => {
    const card = document.createElement("article");
    card.className = "chunk-card";
    card.id = `chunk-card-${chunk.index}`;
    card.dataset.index = String(chunk.index);
    card.classList.toggle("over-budget", !chunk.within_budget);
    card.addEventListener("click", () => activateChunk(chunk.index, true));

    const header = document.createElement("header");
    header.className = "chunk-header";
    const identity = document.createElement("div");
    identity.className = "chunk-identity";
    const number = document.createElement("span");
    number.className = "chunk-number";
    number.textContent = String(chunk.index).padStart(2, "0");
    const id = document.createElement("span");
    id.className = "chunk-id";
    id.textContent = chunk.id;
    identity.append(number, id);

    const stats = document.createElement("div");
    stats.className = "chunk-stats";
    const size = document.createElement("span");
    size.textContent = `${chunk.runes}r`;
    size.classList.toggle("budget-fail", !chunk.within_budget);
    const overlap = document.createElement("span");
    overlap.textContent = `↔ ${chunk.actual_overlap}`;
    const mapped = document.createElement("span");
    mapped.textContent = chunk.mapped ? "mapped" : "structural";
    stats.append(size, overlap, mapped);
    header.append(identity, stats);

    const content = document.createElement("pre");
    content.className = "chunk-content";
    const chunkRunes = Array.from(chunk.content);
    if (chunk.actual_overlap > 0) {
      const prefix = document.createElement("mark");
      prefix.className = "chunk-overlap";
      prefix.textContent = chunkRunes.slice(0, chunk.actual_overlap).join("");
      content.append(prefix, document.createTextNode(chunkRunes.slice(chunk.actual_overlap).join("")));
    } else {
      content.textContent = chunk.content;
    }

    const metadata = document.createElement("pre");
    metadata.className = "chunk-metadata";
    metadata.hidden = !state.metadataVisible;
    metadata.textContent = JSON.stringify(chunk.metadata, null, 2);
    card.append(header, content, metadata);
    elements.chunksList.appendChild(card);
  });
}

function displayedChunks() {
  const chunks = [...state.preview.chunks];
  if (state.preview.summary.size_unit !== "bytes") {
    return chunks;
  }
  return chunks.sort((left, right) => {
    if (left.mapped && right.mapped) {
      return left.source_start - right.source_start || left.index - right.index;
    }
    if (left.mapped) {
      return -1;
    }
    if (right.mapped) {
      return 1;
    }
    return left.index - right.index;
  });
}

function activateChunk(index, scrollPeer = false) {
  state.activeChunk = index;
  document.querySelectorAll(".chunk-card").forEach((card) => {
    card.classList.toggle("active", Number(card.dataset.index) === index);
  });
  updateActiveSource();
  if (scrollPeer && elements.syncScroll.checked && state.sourceTab === "extracted") {
    state.syncGuardUntil = performance.now() + 350;
    const anchor = $(`source-anchor-${index}`);
    if (anchor) {
      elements.sourceMap.scrollTo({ top: Math.max(0, anchor.offsetTop - 90), behavior: "smooth" });
    }
  }
  window.requestAnimationFrame(drawConnectors);
}

function updateActiveSource() {
  document.querySelectorAll(".source-span").forEach((span) => {
    const indexes = (span.dataset.chunks || "").split(",").map(Number);
    span.classList.toggle("active", indexes.includes(state.activeChunk));
  });
}

let connectorFrame = 0;
function queueConnectors() {
  if (connectorFrame) {
    return;
  }
  connectorFrame = window.requestAnimationFrame(() => {
    connectorFrame = 0;
    drawConnectors();
  });
}

function drawConnectors() {
  elements.connectorLayer.replaceChildren();
  if (!state.preview || state.sourceTab !== "extracted" || window.innerWidth <= 900) {
    return;
  }
  const svgRect = elements.connectorLayer.getBoundingClientRect();
  elements.connectorLayer.setAttribute("viewBox", `0 0 ${svgRect.width} ${svgRect.height}`);
  state.preview.chunks.forEach((chunk) => {
    const anchor = $(`source-anchor-${chunk.index}`);
    const card = $(`chunk-card-${chunk.index}`);
    if (!anchor || !card) {
      return;
    }
    const anchorRect = anchor.getBoundingClientRect();
    const cardRect = card.getBoundingClientRect();
    if (cardRect.bottom < svgRect.top || cardRect.top > svgRect.bottom ||
        anchorRect.bottom < svgRect.top || anchorRect.top > svgRect.bottom) {
      return;
    }
    const startX = anchorRect.right - svgRect.left + 6;
    const startY = anchorRect.top - svgRect.top + 8;
    const endX = cardRect.left - svgRect.left - 6;
    const endY = cardRect.top - svgRect.top + Math.min(30, cardRect.height / 2);
    const span = endX - startX;
    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", `M ${startX} ${startY} C ${startX + span * 0.46} ${startY}, ${endX - span * 0.46} ${endY}, ${endX} ${endY}`);
    path.setAttribute("class", `connector-path${chunk.index === state.activeChunk ? " active" : ""}`);
    elements.connectorLayer.appendChild(path);
  });
}

function nearestVisibleIndex(container, selector) {
  const containerRect = container.getBoundingClientRect();
  const targetY = containerRect.top + Math.min(90, containerRect.height * 0.2);
  let nearest = null;
  let distance = Number.POSITIVE_INFINITY;
  document.querySelectorAll(selector).forEach((node) => {
    const rect = node.getBoundingClientRect();
    if (rect.bottom < containerRect.top || rect.top > containerRect.bottom) {
      return;
    }
    const current = Math.abs(rect.top - targetY);
    if (current < distance) {
      distance = current;
      nearest = Number(node.dataset.index ?? node.dataset.chunk);
    }
  });
  return nearest;
}

function syncFromSource() {
  queueConnectors();
  if (!elements.syncScroll.checked || performance.now() < state.syncGuardUntil) {
    return;
  }
  const index = nearestVisibleIndex(elements.sourceMap, ".source-anchor");
  if (index === null || index === state.activeChunk) {
    return;
  }
  state.activeChunk = index;
  document.querySelectorAll(".chunk-card").forEach((card) => {
    card.classList.toggle("active", Number(card.dataset.index) === index);
  });
  updateActiveSource();
  const card = $(`chunk-card-${index}`);
  if (card) {
    state.syncGuardUntil = performance.now() + 200;
    elements.chunksList.scrollTo({ top: Math.max(0, card.offsetTop - 90), behavior: "smooth" });
  }
}

function syncFromChunks() {
  queueConnectors();
  if (!elements.syncScroll.checked || performance.now() < state.syncGuardUntil ||
      state.sourceTab !== "extracted") {
    return;
  }
  const index = nearestVisibleIndex(elements.chunksList, ".chunk-card");
  if (index === null || index === state.activeChunk) {
    return;
  }
  state.activeChunk = index;
  document.querySelectorAll(".chunk-card").forEach((card) => {
    card.classList.toggle("active", Number(card.dataset.index) === index);
  });
  updateActiveSource();
  const anchor = $(`source-anchor-${index}`);
  if (anchor) {
    state.syncGuardUntil = performance.now() + 200;
    elements.sourceMap.scrollTo({ top: Math.max(0, anchor.offsetTop - 90), behavior: "smooth" });
  }
}

function updatePreviewContext() {
  elements.previewContext.replaceChildren();
  const label = document.createElement("span");
  label.textContent = "Preview snapshot";
  const title = document.createElement("strong");
  const detail = document.createElement("small");
  if (state.preview) {
    title.textContent = `${state.preview.input.name} · ${state.preview.summary.total} chunks`;
    detail.textContent = `${state.preview.summary.strategy} · ${state.preview.config.chunk_size} runes · preview ${shortID(state.preview.id)}`;
  } else {
    title.textContent = "No preview available";
    detail.textContent = "Return to Pipeline and run a preview first.";
  }
  elements.previewContext.append(label, title, detail);
}

function updateStoreFields() {
  const pgvector = elements.storeType.value === "pgvector";
  elements.pgvectorFields.hidden = !pgvector;
  [...elements.searchMode.options].forEach((option) => {
    option.disabled = !pgvector && ["hybrid", "keyword"].includes(option.value);
  });
  if (!pgvector && ["hybrid", "keyword"].includes(elements.searchMode.value)) {
    elements.searchMode.value = "vector";
  }
}

async function buildIndex() {
  if (!state.preview) {
    showToast("Run a preview first.", true);
    return;
  }
  const dimensions = Number(elements.embedderDimensions.value);
  const request = {
    preview_id: state.preview.id,
    store: {
      type: elements.storeType.value,
      dsn: elements.pgvectorDSN.value.trim(),
      table: elements.pgvectorTable.value.trim(),
      dimensions,
      enable_text_search: elements.enableTextSearch.checked,
      fusion: elements.fusionMode.value,
    },
    embedder: {
      base_url: elements.embedderBaseURL.value.trim(),
      api_key: elements.embedderAPIKey.value,
      model: elements.embedderModel.value.trim(),
      dimensions,
    },
    top_k: Number(elements.rerankerTopK.value),
    recreate: elements.recreateIndex.checked,
    source_sync: false,
  };
  if (!request.embedder.model || !Number.isInteger(dimensions) || dimensions <= 0) {
    showToast("Embedding model and positive dimensions are required.", true);
    return;
  }
  setBusy(elements.buildIndexButton, true, "Building index");
  elements.indexStatus.textContent = "Embedding preview chunks and calling knowledge.Load…";
  elements.indexStatus.classList.remove("error");
  try {
    state.index = await api("/api/index", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    renderIndex();
    showToast(`Indexed ${state.index.documents} documents in ${state.index.duration_ms} ms.`);
  } catch (error) {
    elements.indexStatus.textContent = error.message;
    elements.indexStatus.classList.add("error");
    showToast(error.message, true);
  } finally {
    setBusy(elements.buildIndexButton, false, "Build index");
    elements.buildIndexButton.disabled = !state.preview;
  }
}

function renderIndex() {
  const index = state.index;
  elements.indexBadge.classList.add("ready");
  elements.indexBadge.replaceChildren();
  const label = document.createElement("span");
  label.textContent = "Index ready";
  const title = document.createElement("strong");
  title.textContent = `${index.store} · ${index.documents} documents`;
  const detail = document.createElement("small");
  detail.textContent = `${index.duration_ms} ms · ${index.table || "ephemeral memory"} · ${shortID(index.id)}`;
  elements.indexBadge.append(label, title, detail);
  elements.indexStatus.textContent = `${index.framework_path} · ${index.search_modes.join(", ")}`;
  elements.searchStatus.textContent = "Ready to run knowledge.Search.";
  elements.searchButton.disabled = false;
  [...elements.searchMode.options].forEach((option) => {
    option.disabled = !index.search_modes.includes(option.value);
  });
  if (!index.search_modes.includes(elements.searchMode.value)) {
    elements.searchMode.value = index.search_modes[0];
  }
}

async function runSearch() {
  if (!state.index) {
    showToast("Build an index first.", true);
    return;
  }
  let metadata = {};
  const filter = elements.metadataFilter.value.trim();
  if (filter) {
    try {
      metadata = JSON.parse(filter);
    } catch (error) {
      showToast("Metadata filter must be valid JSON.", true);
      return;
    }
    if (!metadata || Array.isArray(metadata) || typeof metadata !== "object") {
      showToast("Metadata filter must be a JSON object.", true);
      return;
    }
  }
  const request = {
    index_id: state.index.id,
    query: elements.searchQuery.value.trim(),
    mode: elements.searchMode.value,
    max_results: Number(elements.maxResults.value),
    min_score: Number(elements.minScore.value),
    metadata,
  };
  if (!request.query) {
    showToast("Enter a search query.", true);
    return;
  }
  setBusy(elements.searchButton, true, "Searching");
  elements.searchStatus.textContent = "Calling knowledge.Search…";
  try {
    const response = await api("/api/search", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(request),
    });
    renderSearchResults(response);
    elements.searchStatus.textContent = `${response.results.length} results · ${response.mode} · ${response.duration_ms} ms`;
  } catch (error) {
    elements.searchStatus.textContent = error.message;
    showToast(error.message, true);
  } finally {
    setBusy(elements.searchButton, false, "Run search");
    elements.searchButton.disabled = !state.index;
  }
}

function renderSearchResults(response) {
  elements.searchResults.replaceChildren();
  elements.searchResults.classList.toggle("empty", response.results.length === 0);
  if (response.results.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-state retrieval-empty";
    const title = document.createElement("h4");
    title.textContent = "No documents passed the search constraints";
    const detail = document.createElement("p");
    detail.textContent = "Try lowering minimum score, removing metadata filters, or changing search mode.";
    empty.append(title, detail);
    elements.searchResults.appendChild(empty);
    return;
  }
  response.results.forEach((result) => {
    const card = document.createElement("article");
    card.className = "result-card";
    const rank = document.createElement("div");
    rank.className = "result-rank";
    rank.textContent = `#${result.rank}`;
    const body = document.createElement("div");
    body.className = "result-body";
    const id = document.createElement("h4");
    id.textContent = result.document?.id || "document";
    const content = document.createElement("pre");
    content.textContent = result.document?.content || "";
    body.append(id, content);
    const trace = document.createElement("aside");
    trace.className = "result-trace";
    trace.append(
      scoreLine("Final score", formatScore(result.score)),
      scoreLine("Dense", formatScore(result.dense_score)),
      scoreLine("Sparse", formatScore(result.sparse_score)),
    );
    const metadata = document.createElement("pre");
    metadata.textContent = JSON.stringify(result.document?.metadata || {}, null, 2);
    trace.appendChild(metadata);
    card.append(rank, body, trace);
    elements.searchResults.appendChild(card);
  });
}

function scoreLine(label, value) {
  const row = document.createElement("div");
  row.className = "score-line";
  const name = document.createElement("span");
  name.textContent = label;
  const score = document.createElement("strong");
  score.textContent = value;
  row.append(name, score);
  return row;
}

function copyGoConfig() {
  if (!state.preview) {
    return;
  }
  const config = state.preview.config;
  const constructors = {
    fixed: `chunking.NewFixedSizeChunking(\n\tchunking.WithChunkSize(${config.chunk_size}),\n\tchunking.WithOverlap(${config.overlap}),\n)`,
    recursive: `chunking.NewRecursiveChunking(\n\tchunking.WithRecursiveChunkSize(${config.chunk_size}),\n\tchunking.WithRecursiveOverlap(${config.overlap}),\n)`,
    markdown: `chunking.NewMarkdownChunking(\n\tchunking.WithMarkdownChunkSize(${config.chunk_size}),\n\tchunking.WithMarkdownOverlap(${config.overlap}),\n)`,
    json: `chunking.NewJSONChunking(\n\tchunking.WithJSONChunkSize(${config.chunk_size}),\n)`,
  };
  let snippet = `// Reader: ${state.preview.summary.reader}\n// Size unit: Unicode runes\n`;
  if (config.strategy === "reader") {
    snippet += `// Reader default strategy: ${state.preview.summary.strategy}\n`;
  } else {
    snippet += `strategy := ${constructors[config.strategy]}\n`;
  }
  if (config.extractor === "docling") {
    snippet += `\nextractor := docling.New(\n\tdocling.WithEndpoint(${JSON.stringify(config.docling_endpoint)}),\n\tdocling.WithOCR(${config.ocr_enabled}),\n\tdocling.WithImageRefMode(docling.ImageRefMode(${JSON.stringify(config.image_mode)})),\n)\n`;
  }
  navigator.clipboard.writeText(snippet)
    .then(() => showToast("Go configuration copied."))
    .catch(() => showToast("Clipboard access was denied.", true));
}

function formatBytes(bytes) {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KiB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}

function formatScore(value) {
  if (value === null || value === undefined || value === "") {
    return "—";
  }
  const number = Number(value);
  return Number.isFinite(number) ? number.toFixed(5) : String(value);
}

function shortID(value) {
  return value.length > 18 ? `${value.slice(0, 18)}…` : value;
}

function bindEvents() {
  document.querySelectorAll(".nav-button").forEach((button) => {
    button.addEventListener("click", () => switchView(button.dataset.view));
  });
  elements.sampleSelect.addEventListener("change", () => loadSample(elements.sampleSelect.value));
  elements.fileInput.addEventListener("change", () => handleUpload().catch((error) => showToast(error.message, true)));
  elements.resetInputButton.addEventListener("click", () => loadSample(elements.sampleSelect.value));
  elements.sourceEditor.addEventListener("input", () => {
    if (!elements.sourceEditor.readOnly) {
      state.currentFile = null;
      elements.inputState.textContent = "Edited";
      invalidatePreview();
    }
  });
  elements.extractorSelect.addEventListener("change", () => {
    updateExtractorFields();
    invalidatePreview();
  });
  elements.strategySelect.addEventListener("change", () => {
    updateStrategyFields();
    invalidatePreview();
  });
  [
    elements.readerSelect,
    elements.chunkSize,
    elements.overlap,
    elements.doclingEndpoint,
    elements.outputFormat,
    elements.imageMode,
    elements.ocrEnabled,
  ].forEach((control) => control.addEventListener("change", invalidatePreview));
  elements.previewButton.addEventListener("click", runPreview);
  elements.copyConfigButton.addEventListener("click", copyGoConfig);
  elements.metadataButton.addEventListener("click", () => {
    state.metadataVisible = !state.metadataVisible;
    elements.metadataButton.classList.toggle("active", state.metadataVisible);
    document.querySelectorAll(".chunk-metadata").forEach((metadata) => {
      metadata.hidden = !state.metadataVisible;
    });
    queueConnectors();
  });
  document.querySelectorAll("[data-source-tab]").forEach((button) => {
    button.addEventListener("click", () => {
      state.sourceTab = button.dataset.sourceTab;
      renderSourceTab();
    });
  });
  elements.openRetrievalButton.addEventListener("click", () => switchView("retrieval"));
  elements.storeType.addEventListener("change", updateStoreFields);
  elements.buildIndexButton.addEventListener("click", buildIndex);
  elements.searchButton.addEventListener("click", runSearch);
  elements.searchQuery.addEventListener("keydown", (event) => {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      runSearch();
    }
  });
  elements.sourceMap.addEventListener("scroll", syncFromSource, { passive: true });
  elements.chunksList.addEventListener("scroll", syncFromChunks, { passive: true });
  elements.syncScroll.addEventListener("change", queueConnectors);
  window.addEventListener("resize", queueConnectors);
}

async function init() {
  bindEvents();
  updateExtractorFields();
  updateStrategyFields();
  updateStoreFields();
  await Promise.all([checkHealth(), loadSamples()]);
}

init().catch((error) => {
  showToast(`Initialization failed: ${error.message}`, true);
  elements.previewStatus.textContent = error.message;
  elements.previewStatus.classList.add("error");
});
