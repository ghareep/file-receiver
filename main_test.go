package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// uploadHandler is constructed with immutable captured config (dir, maxBytes,
// token), so it is safe for concurrent use; these tests exercise that too via
// -race.

func multipartBody(t *testing.T, filename string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	const boundary = "test-boundary"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"file\"; filename=\"" + filename + "\"\r\n")
	buf.WriteString("Content-Type: application/octet-stream\r\n\r\n")
	buf.Write(content)
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	return &buf, "multipart/form-data; boundary=" + boundary
}

func postUpload(t *testing.T, h http.HandlerFunc, filename string, content []byte, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, filename, content)
	req := httptest.NewRequest(http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	if token != "" {
		req.Header.Set("X-Upload-Token", token)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestUploadHappyPath(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 1<<20, "")
	content := []byte("payload-bytes-0123456789")

	rec := postUpload(t, h, "report.pdf", content, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "report.pdf"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}
	fi, err := os.Stat(filepath.Join(dir, "report.pdf"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Fatalf("file perms = %o, want 0600", perm)
	}
}

func TestUploadMethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 1<<20, "")
	req := httptest.NewRequest(http.MethodGet, "/upload", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /upload = %d, want 405", rec.Code)
	}
}

func TestUploadMissingFileField(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 1<<20, "")
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing file = %d, want 400", rec.Code)
	}
}

func TestUploadExceedsMaxBytes(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 16, "") // cap at 16 bytes
	rec := postUpload(t, h, "big.bin", bytes.Repeat([]byte("A"), 1024), "")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413; body=%q", rec.Code, rec.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("oversized upload left %d files behind; want 0", len(entries))
	}
}

func TestUploadTokenAuth(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 1<<20, "s3cret")

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "wrong", http.StatusUnauthorized},
		{"correct token", "s3cret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postUpload(t, h, "x.bin", []byte("x"), tc.token)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestUploadDuplicateFilenamesGetUniquePaths(t *testing.T) {
	dir := t.TempDir()
	h := uploadHandler(dir, 1<<20, "")

	if rec := postUpload(t, h, "report.pdf", []byte("first"), ""); rec.Code != http.StatusOK {
		t.Fatalf("first upload = %d", rec.Code)
	}
	if rec := postUpload(t, h, "report.pdf", []byte("second"), ""); rec.Code != http.StatusOK {
		t.Fatalf("second upload = %d", rec.Code)
	}

	first, err := os.ReadFile(filepath.Join(dir, "report.pdf"))
	if err != nil {
		t.Fatalf("report.pdf missing: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "report-1.pdf"))
	if err != nil {
		t.Fatalf("report-1.pdf missing: %v", err)
	}
	if string(first) != "first" || string(second) != "second" {
		t.Fatalf("unique-path contents wrong: first=%q second=%q", first, second)
	}
}

func TestSaveUploadAtomicAbortOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	err := saveUpload(context.Background(), iotest.ErrReader(errors.New("connection reset")), dir, "x.bin")
	if err == nil {
		t.Fatal("saveUpload with failing reader: want error, got nil")
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("readdir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("aborted upload left %d files; want 0 (temp file must be removed)", len(entries))
	}
}

func TestUniquePath(t *testing.T) {
	dir := t.TempDir()

	// Empty dir: base name returned unchanged.
	p, err := uniquePath(dir, "Makefile")
	if err != nil || filepath.Base(p) != "Makefile" {
		t.Fatalf("uniquePath empty dir = %q, %v", p, err)
	}

	for _, existing := range []string{"report.pdf", "report-1.pdf"} {
		if err := os.WriteFile(filepath.Join(dir, existing), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	p, err = uniquePath(dir, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(p); got != "report-2.pdf" {
		t.Fatalf("uniquePath with collisions = %q, want report-2.pdf", got)
	}

	// Extensionless collision.
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = uniquePath(dir, "Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(p); got != "Makefile-1" {
		t.Fatalf("uniquePath extensionless collision = %q, want Makefile-1", got)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":        "report.pdf",
		"../evil.txt":       "evil.txt",
		"a/b/c.txt":         "c.txt",
		"":                  "uploaded_file.bin",
		".":                 "uploaded_file.bin",
		"..":                "uploaded_file.bin",
		"  spaced.txt  ":    "spaced.txt",
		"/absolute/run.bin": "run.bin",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIndexHandler(t *testing.T) {
	h := indexHandler("http://192.0.2.1:8080")

	t.Run("serves page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "File Receiver") {
			t.Fatal("index body missing expected marker")
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("Content-Type = %q", ct)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
			t.Fatal("missing Content-Security-Policy header")
		}
	})

	t.Run("rejects POST", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST / = %d, want 405", rec.Code)
		}
	})
}
