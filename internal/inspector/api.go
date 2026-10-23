package inspector

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed ui/index.html
var indexHTML []byte

// pingInterval is how often the SSE stream sends a keepalive comment.
var pingInterval = 15 * time.Second

const maxEditBody = 16 << 20

// UI returns the loopback-only HTTP handler for the inspector page and API.
func (in *Inspector) UI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		t, _ := in.target.Load().(string)
		writeJSON(w, 200, struct {
			PublicURL string `json:"public_url"`
			Target    string `json:"target"`
		}{in.base(), t})
	})
	mux.HandleFunc("GET /api/requests", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, in.store.List())
	})
	mux.HandleFunc("DELETE /api/requests", func(w http.ResponseWriter, r *http.Request) {
		in.store.Clear()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, ok := in.lookup(r)
		if !ok {
			http.Error(w, ErrNotFound.Error(), 404)
			return
		}
		writeJSON(w, 200, e)
	})
	mux.HandleFunc("GET /api/requests/{id}/curl", func(w http.ResponseWriter, r *http.Request) {
		e, ok := in.lookup(r)
		if !ok {
			http.Error(w, ErrNotFound.Error(), 404)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, CurlCommand(e, in.base()))
	})
	mux.HandleFunc("POST /api/requests/{id}/replay", in.handleReplay)
	mux.HandleFunc("GET /api/events", in.handleEvents)
	return guard(mux)
}

func (in *Inspector) lookup(r *http.Request) (*Exchange, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, false
	}
	return in.store.Get(id)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (in *Inspector) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, ErrNotFound.Error(), 404)
		return
	}
	var edit *Edit
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEditBody))
	if err != nil {
		http.Error(w, "bad request body", 400)
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		edit = &Edit{}
		if err := json.Unmarshal(raw, edit); err != nil {
			http.Error(w, "bad edit: "+err.Error(), 400)
			return
		}
	}
	newID, err := in.Replay(r.Context(), id, edit)
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, err.Error(), 404)
	case errors.Is(err, ErrTruncated):
		http.Error(w, err.Error(), 409)
	case errors.Is(err, ErrBadEdit):
		http.Error(w, err.Error(), 400)
	case err != nil:
		http.Error(w, err.Error(), 500)
	default:
		writeJSON(w, 200, map[string]int64{"id": newID})
	}
}

func (in *Inspector) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	ch, cancel := in.store.Subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if rc.Flush() != nil {
		return
	}
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case s, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(s)
			if _, err := fmt.Fprintf(w, "event: exchange\ndata: %s\n\n", b); err != nil {
				return
			}
		case <-t.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func isLoopbackHost(hostport string) bool {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// guard blocks DNS-rebinding, cross-origin and form-based CSRF requests. It sets no CORS headers.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
