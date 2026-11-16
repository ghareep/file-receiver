// Command file-receiver is a minimal LAN file-upload server.
//
// It serves a single-page upload form on / and accepts POST /upload with a
// multipart "file" field. Uploads are streamed to a temp file, fsynced, and
// atomically renamed into the upload directory so a crash can never leave a
// half-written file masquerading as a complete one.
//
// Security posture: designed for a trusted LAN. HTTP is cleartext; enable the
// optional -token flag (constant-time compared, sent via X-Upload-Token) on
// any network you do not fully control.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultMaxMB      = 500
	shutdownTimeout   = 15 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// pageTmpl is parsed exactly once at process start. A parse failure here is a
// programmer error (the template is a compile-time constant), so panicking at
// init is the correct failure mode.
var pageTmpl = template.Must(template.New("page").Parse(htmlPage))

func main() {
	addr := flag.String("addr", ":8080", "listen address (host:port or :port)")
	dir := flag.String("dir", defaultUploadDir(), "directory to store uploads")
	maxMB := flag.Int("max-mb", defaultMaxMB, "maximum accepted upload size in MiB")
	token := flag.String("token", "", "optional shared secret; clients must send it in X-Upload-Token")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -log-level %q: %v\n", *logLevel, err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if *maxMB <= 0 {
		fmt.Fprintln(os.Stderr, "-max-mb must be positive")
		os.Exit(2)
	}
	maxBytes := int64(*maxMB) * 1024 * 1024

	if err := os.MkdirAll(*dir, 0700); err != nil {
		slog.Error("create upload dir", "dir", *dir, "err", err)
		os.Exit(1)
	}

	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		slog.Error("invalid -addr", "addr", *addr, "err", err)
		os.Exit(2)
	}
	if host == "" {
		host = getLocalIP()
	}
	url := fmt.Sprintf("http://%s:%s", host, port)

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler(url))
	mux.HandleFunc("/upload", uploadHandler(*dir, maxBytes, *token))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// ReadTimeout intentionally unset: upload duration is client-bound and
		// unbounded read deadlines would kill large transfers. Intake is
		// bounded instead by http.MaxBytesReader in uploadHandler.
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server started", "addr", *addr, "url", url, "dir", *dir, "max_mib", *maxMB, "auth", *token != "")
		fmt.Printf("QR Code: qrencode -t ANSIUTF8 '%s'\n", url)
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case s := <-sigCh:
		slog.Info("shutdown signal received", "signal", s)
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("graceful shutdown failed", "err", err)
		}
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server exited", "err", err)
			os.Exit(1)
		}
	}
	slog.Info("server stopped")
}

// indexHandler serves the upload form. url is the address advertised to the
// client; it is captured at startup, making the handler stateless and safe for
// concurrent use.
func indexHandler(url string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The page is a single self-contained inline document; it needs inline
		// script/style and nothing external, so the policy permits exactly that.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if err := pageTmpl.Execute(w, url); err != nil {
			slog.Error("render index", "err", err)
		}
	}
}

// uploadHandler returns a handler that stores multipart "file" uploads under
// dir. maxBytes caps intake; token, when non-empty, requires a matching
// X-Upload-Token header. All error paths return an accurate status code — a
// failed write is never reported as success.
func uploadHandler(dir string, maxBytes int64, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" && !validToken(r.Header.Get("X-Upload-Token"), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		file, header, err := r.FormFile("file")
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "file exceeds maximum size", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "missing or unreadable file field", http.StatusBadRequest)
			}
			slog.Warn("rejected upload", "err", err)
			return
		}
		defer file.Close()

		name := sanitizeFilename(header.Filename)
		if err := saveUpload(r.Context(), file, dir, name); err != nil {
			slog.Error("upload failed", "name", name, "err", err)
			http.Error(w, "upload failed", http.StatusInternalServerError)
			return
		}

		slog.Info("upload stored", "name", name, "size", header.Size)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := io.WriteString(w, "OK"); err != nil {
			slog.Warn("response write failed", "err", err)
		}
	}
}

// saveUpload streams src into a temp file inside dir, fsyncs it, then
// atomically renames it to a unique final name. On any error the temp file is
// removed, so no partial upload is ever left behind.
func saveUpload(ctx context.Context, src io.Reader, dir, name string) error {
	tmp, err := os.CreateTemp(dir, ".upload-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeded

	// ctxReader lets io.Copy abort promptly when the request context is
	// canceled (client disconnect or server shutdown).
	if _, err := io.Copy(tmp, ctxReader{ctx: ctx, r: src}); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to stream upload %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to flush upload %s to disk: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file for %s: %w", name, err)
	}

	final, err := uniquePath(dir, name)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("failed to finalize %s: %w", final, err)
	}
	// CreateTemp already uses 0600; enforce explicitly so the on-disk result
	// is correct regardless of umask or platform quirks.
	if err := os.Chmod(final, 0600); err != nil {
		return fmt.Errorf("failed to secure permissions on %s: %w", final, err)
	}
	return nil
}

// uniquePath returns a path for name inside dir that does not clobber an
// existing file, appending -1, -2, ... before the extension as needed.
func uniquePath(dir, name string) (string, error) {
	base := filepath.Base(name)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("invalid filename %q", name)
	}
	candidate := filepath.Join(dir, base)
	if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate, nil
	} else if err != nil {
		return "", fmt.Errorf("failed to stat %s: %w", candidate, err)
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("failed to stat %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("unable to allocate unique name for %s", base)
}

// sanitizeFilename reduces a client-supplied name to a safe single path
// element: path separators and traversal components are stripped by Base,
// and empty/`.`/`..` results fall back to a default name.
func sanitizeFilename(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	switch base {
	case "", ".", "..", string(filepath.Separator):
		return "uploaded_file.bin"
	}
	return base
}

// validToken compares the presented token against the configured one in
// constant time so response timing does not leak the secret.
func validToken(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// defaultUploadDir replaces the fragile "~" expansion with the real home dir.
func defaultUploadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, "Downloads", "mobile_uploads")
}

// ctxReader aborts reads once ctx is done, so an upload stops promptly on
// client disconnect or server shutdown instead of draining a dead socket.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
		return c.r.Read(p)
	}
}

// getLocalIP picks the first non-loopback IPv4 address. With multiple
// interfaces (VPN, Docker, Tailscale) this may pick a non-routable one; the
// -addr flag is the explicit override. Used for display only — the listener
// always binds the address given by -addr.
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "127.0.0.1"
}

const htmlPage = `<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>File Receiver</title>
<style>
body{font-family:sans-serif;margin:0;padding:12px;background:#f0f0f0;text-align:center}
h1{font-size:1.2em;color:#333}
.zone{background:#fff;border:3px dashed #4A90D9;border-radius:10px;padding:40px;margin:12px 0;font-size:.95em;color:#555;cursor:pointer;transition:background .2s}
.zone:hover,.zone:active{background:#e6f2ff}
.status{font-weight:bold;color:#4A90D9;margin-top:10px}
.progress-wrap{width:100%;max-width:400px;margin:10px auto;background:#ddd;border-radius:8px;overflow:hidden}
.progress-bar{height:24px;background:#4A90D9;width:0%;color:#fff;font-size:.85em;line-height:24px;text-align:center}
</style>
</head>
<body>
<h1>Mobile File Upload</h1>
<div class="zone" onclick="document.getElementById('fileInput').click()">Tap here to choose a file from your phone</div>
<input type="file" id="fileInput" name="file" style="display:none" onchange="uploadFile(this)">
<div class="status" id="status">Waiting for file...</div>
<div class="progress-wrap"><div class="progress-bar" id="progressBar">0%</div></div>
<script>
function uploadFile(input){
  var f=input.files[0];
  if(!f){document.getElementById('status').textContent='No file selected';return;}
  document.getElementById('status').textContent='Uploading: '+f.name+'...';
  document.getElementById('progressBar').style.width='0%';
  document.getElementById('progressBar').textContent='0%';
  var xhr=new XMLHttpRequest();
  var fd=new FormData();
  fd.append('file',f);
  xhr.upload.onprogress=function(e){
    if(e.lengthComputable){
      var pct=Math.round((e.loaded/e.total)*100);
      document.getElementById('progressBar').style.width=pct+'%';
      document.getElementById('progressBar').textContent=pct+'%';
    }
  };
  xhr.onload=function(){
    if(xhr.status===200){
      document.getElementById('status').textContent='Success! Uploaded '+f.name;
    } else {
      document.getElementById('status').textContent='Upload failed (HTTP '+xhr.status+')';
    }
  };
  xhr.onerror=function(){
    document.getElementById('status').textContent='Upload network error';
  };
  xhr.open('POST','/upload');
  xhr.send(fd);
}
</script>
</body>
</html>`
