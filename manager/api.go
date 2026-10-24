package manager

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

//go:embed page
var pageFS embed.FS

// PageFS is the member's page: one static bundle (index.html). In use the
// platform serves it from the package's pinned bundle; this copy is the
// bundle's source and serves it in development (Handler's withPage).
func PageFS() fs.FS {
	sub, _ := fs.Sub(pageFS, "page")
	return sub
}

// Handler is the loopback API behind the per-launch bearer:
//
//	GET  /v1/state                    the snapshot
//	POST /v1/refresh                  look at the computer and for newer versions
//	POST /v1/agents/{id}/{action}     install update rollback remove check
//	                                  register unregister cancel
//
// withPage also serves the page at / (without data, for development).
func (s *Service) Handler(token string, withPage bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Snapshot())
	})
	mux.HandleFunc("POST /v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		s.loadSettings()
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		s.RefreshDetected(ctx)
		go s.CheckAll(context.Background())
		writeJSON(w, http.StatusOK, s.Snapshot())
	})
	mux.HandleFunc("POST /v1/agents/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, action := r.PathValue("id"), r.PathValue("action")
		var err error
		if action == "cancel" {
			err = s.Cancel(id)
		} else {
			err = s.Begin(id, action)
		}
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
		case errors.Is(err, ErrBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	})
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == r.Header.Get("Authorization") || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			next.ServeHTTP(w, r)
		})
	}
	root := http.NewServeMux()
	root.Handle("/v1/", auth(mux))
	if withPage {
		root.Handle("/", http.FileServerFS(PageFS()))
	}
	return root
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
