package viz

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kevin/knowledge-base/internal/store"
)

//go:embed index.html
var indexHTML []byte

// Source provides the data; *store.Store implements it.
type Source interface {
	Snapshot(ctx context.Context, limit int) (*store.Snapshot, error)
}

// Options configures the server.
type Options struct {
	Source Source
	Limit  int // maximum number of entries shown (newest first)
}

type docJSON struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Project      string   `json:"project"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	SupersededBy *int64   `json:"supersededBy"`
	Source       string   `json:"source"`
	Tags         []string `json:"tags"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	Content      string   `json:"content"`
	X            float64  `json:"x"`
	Y            float64  `json:"y"`
}

type dataJSON struct {
	GeneratedAt string    `json:"generatedAt"`
	Model       string    `json:"model"`
	DBBytes     int64     `json:"dbBytes"`
	Total       int       `json:"total"`
	Shown       int       `json:"shown"`
	Projects    []string  `json:"projects"`
	Docs        []docJSON `json:"docs"`
}

// Handler returns the HTTP handler (page + JSON API). It only serves requests whose
// Host header is a loopback name, which blocks DNS-rebinding attacks on the local server.
func Handler(o Options) http.Handler {
	if o.Limit <= 0 {
		o.Limit = 5000
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		w.Write(indexHTML)
	})
	mux.HandleFunc("/api/data", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		d, err := build(ctx, o)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(d)
	})
	return loopbackOnly(mux)
}

func build(ctx context.Context, o Options) (*dataJSON, error) {
	snap, err := o.Source.Snapshot(ctx, o.Limit)
	if err != nil {
		return nil, err
	}
	vecs := make([][]float32, len(snap.Docs))
	for i, d := range snap.Docs {
		vecs[i] = d.Vec
	}
	pts := Project2D(vecs)
	out := &dataJSON{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Model:       snap.Model,
		DBBytes:     snap.DBBytes,
		Total:       snap.Total,
		Shown:       len(snap.Docs),
		Docs:        make([]docJSON, len(snap.Docs)),
	}
	seen := map[string]bool{}
	for i, d := range snap.Docs {
		content := d.Content
		if r := []rune(content); len(r) > 4000 {
			content = string(r[:4000]) + "…"
		}
		out.Docs[i] = docJSON{
			ID: d.ID, Title: d.Title, Project: d.Project, Kind: d.Kind, Status: d.Status,
			SupersededBy: d.SupersededBy, Source: d.Source, Tags: nonNil(d.Tags),
			CreatedAt: d.CreatedAt.Format(time.RFC3339), UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
			Content: content, X: pts[i][0], Y: pts[i][1],
		}
		if d.Project != "" && !seen[d.Project] {
			seen[d.Project] = true
			out.Projects = append(out.Projects, d.Project)
		}
	}
	sort.Strings(out.Projects)
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve listens on addr (loopback only) until ctx is cancelled. ready receives the
// actual address once listening.
func Serve(ctx context.Context, addr string, o Options, ready func(addr string)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("the visualisation has no authentication and only listens on loopback addresses")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(o), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	if ready != nil {
		ready(ln.Addr().String())
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
