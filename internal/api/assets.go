package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// asset is one embedded file, prepared once at startup.
type asset struct {
	contentType string
	raw         []byte
	gz          []byte // nil when compression does not pay off
	etag        string
}

// assetHandler serves the embedded UI.
//
// http.FileServer cannot help here: files from embed.FS carry a zero modtime,
// so ServeContent emits neither Last-Modified nor ETag and every reload
// re-downloads the whole bundle with a 200. Over a slow link that is the
// entire cost of opening the page. Hashing each file once at startup gives a
// stable validator, so a reload transfers nothing.
type assetHandler struct {
	files map[string]*asset
}

func newAssetHandler(fsys fs.FS) (*assetHandler, error) {
	h := &assetHandler{files: map[string]*asset{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		a := &asset{
			contentType: ct,
			raw:         b,
			etag:        `"` + hex.EncodeToString(sum[:12]) + `"`,
		}
		// Compression is worth it for the text bundle and pointless for
		// anything already compact or already compressed.
		if len(b) > 1024 && compressible(ct) {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			if _, err := zw.Write(b); err == nil && zw.Close() == nil &&
				buf.Len() < len(b)*9/10 {
				a.gz = append([]byte(nil), buf.Bytes()...)
			}
		}
		h.files[p] = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := h.files["index.html"]; !ok {
		return nil, fmt.Errorf("web assets are missing index.html")
	}
	return h, nil
}

func compressible(ct string) bool {
	switch {
	case strings.HasPrefix(ct, "text/"),
		strings.Contains(ct, "javascript"),
		strings.Contains(ct, "json"),
		strings.Contains(ct, "svg"):
		return true
	}
	return false
}

func (h *assetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		name = "index.html"
	}
	a, ok := h.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	head := w.Header()
	head.Set("Content-Type", a.contentType)
	head.Set("ETag", a.etag)
	// Revalidate every time but transfer nothing when unchanged. The bundle
	// has no content hash in its name, so it must not be cached blindly.
	head.Set("Cache-Control", "no-cache")
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Referrer-Policy", "no-referrer")
	head.Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := a.raw
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		head.Set("Content-Encoding", "gzip")
		head.Add("Vary", "Accept-Encoding")
		body = a.gz
	}
	head.Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// etagMatches handles the comma-separated list form and the "*" wildcard.
func etagMatches(header, etag string) bool {
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}

var _ = io.Discard
