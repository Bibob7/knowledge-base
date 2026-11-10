// Package store keeps the knowledge base: documents and facts, their chunks with
// embeddings, the supersede history, and hybrid search (vector + full text).
//
// Store is implemented for SQLite (a single file, the default) and for Postgres with
// pgvector (e.g. a shared team server); Open picks the implementation from the
// database URL: sqlite:///path/to/knowledge.db or postgres://...
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store is a knowledge base backend. All implementations behave the same; the
// contract tests in store_contract_test.go run against each of them.
type Store interface {
	// AddDocument stores a document. If an active one with the same source already
	// exists, it is replaced (re-ingest). Entries in Supersedes are marked as
	// outdated (and kept as history).
	AddDocument(ctx context.Context, in DocumentInput) (AddResult, error)
	// Supersede marks oldIDs as replaced by newID, which must be active. It returns the
	// IDs that were actually changed.
	Supersede(ctx context.Context, oldIDs []int64, newID int64) ([]int64, error)
	// Similar returns the most similar active entries (best chunk per document) within
	// the project plus the global entries; without project it looks at everything.
	Similar(ctx context.Context, embedding []float32, project string, exclude []int64, limit int) ([]SimilarEntry, error)
	// Search combines vector and full-text search via Reciprocal Rank Fusion
	// (k=60, top 50 of each), over active entries of the project plus global ones.
	Search(ctx context.Context, query string, embedding []float32, project string, tags []string, limit int) ([]SearchHit, error)
	ListDocuments(ctx context.Context, tag, project string, includeSuperseded bool, limit int) ([]DocumentInfo, error)
	// GetDocument reconstructs the text from the chunks (overlaps included).
	GetDocument(ctx context.Context, id int64) (*Document, error)
	DeleteDocument(ctx context.Context, id int64) error
	// Snapshot returns the newest `limit` entries including their document embeddings
	// (the mean of the chunk embeddings), for the visualisation.
	Snapshot(ctx context.Context, limit int) (*Snapshot, error)
	// RecordUse counts that the entries were used in a session: injected by the prompt
	// hook or retrieved by Claude. Unknown IDs are ignored.
	RecordUse(ctx context.Context, ids []int64, kind UseKind) error
	// UsageSince reports when usage tracking started (zero if it has not).
	UsageSince(ctx context.Context) (time.Time, error)
	// Unused returns active entries (of the project plus global ones; without project all)
	// that were created before `before` and have not been used since, least recently
	// used first.
	Unused(ctx context.Context, project string, before time.Time, limit int) ([]DocumentInfo, error)
	// CurrentEmbedding reports the dimension of the stored vectors and the model they
	// were created with (empty if unknown).
	CurrentEmbedding(ctx context.Context) (dim int, model string, err error)
	// Reindex re-embeds every chunk (including those of superseded entries) with the
	// configured model. IDs, history and all other data stay untouched; an interrupted
	// run can simply be started again.
	Reindex(ctx context.Context, o ReindexOptions) error
	Close()
}

// Open connects to the database, creates or migrates the schema and verifies that the
// stored vectors were made with the configured model and dimension.
func Open(ctx context.Context, url string, dim int, model string) (Store, error) {
	switch {
	case isPostgresURL(url):
		return openPostgres(ctx, url, dim, model)
	case isSQLiteURL(url):
		return openSQLite(ctx, url, dim, model)
	default:
		return nil, unsupportedURL(url)
	}
}

// OpenReadOnly connects without migrating or checking the schema (fast, for hooks and
// other short-lived commands; the MCP server has opened the database with Open before).
// Despite the name such a store can write: if model is not empty, every AddDocument
// first checks inside its transaction that the database is still embedded with that
// model and dimension (it may have been reindexed meanwhile), otherwise writes are not
// checked.
func OpenReadOnly(ctx context.Context, url string, dim int, model string) (Store, error) {
	var (
		s   Store
		err error
	)
	switch {
	case isPostgresURL(url):
		var pg *pgStore
		if pg, err = openPostgresReadOnly(ctx, url, dim); err == nil {
			pg.model = normalizeModel(model)
			s = pg
		}
	case isSQLiteURL(url):
		var lite *sqliteStore
		if lite, err = openSQLiteReadOnly(ctx, url, dim); err == nil {
			lite.model = normalizeModel(model)
			s = lite
		}
	default:
		err = unsupportedURL(url)
	}
	return s, err
}

// errEmbeddingChanged is returned by a write when the database was re-embedded with a
// different model or dimension after this store was opened.
func errEmbeddingChanged(storedDim, storedModel string, dim int, model string) error {
	return fmt.Errorf("the database is now embedded with model %q (dimension %s), but this process uses %q (dimension %d); "+
		"nothing was stored (restart the MCP server via /mcp, or check EMBEDDING_MODEL)", storedModel, storedDim, model, dim)
}

func isPostgresURL(url string) bool {
	return strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://")
}

func unsupportedURL(url string) error {
	scheme, _, ok := strings.Cut(url, "://")
	if !ok {
		scheme = url
	}
	return fmt.Errorf("unsupported DATABASE_URL %q (expected sqlite:///path or postgres://...)", scheme)
}

var ErrNotFound = errors.New("document not found")

func normalizeModel(m string) string { return strings.TrimSuffix(strings.TrimSpace(m), ":latest") }

// ChunkInput is a piece of text together with its embedding.
type ChunkInput struct {
	Content   string
	Embedding []float32
}

// DocumentInput describes a new document or a new fact.
type DocumentInput struct {
	Title      string
	Source     string
	Project    string // empty = cross-project
	Kind       string // "document" or "fact"
	Tags       []string
	Supersedes []int64 // active entries that this one replaces
	Chunks     []ChunkInput
}

type AddResult struct {
	ID         int64
	Replaced   bool    // an existing document with the same source was replaced
	Superseded []int64 // entries that were actually marked as outdated
}

type SimilarEntry struct {
	ID         int64
	Title      string
	Project    string
	Kind       string
	Content    string
	Similarity float64
}

type SearchHit struct {
	DocumentID int64
	Title      string
	Source     string
	Project    string
	Kind       string
	Tags       []string
	ChunkIndex int
	Content    string
	Score      float64
	Sim        float64 // cosine similarity of the chunk to the query (absolute, unlike Score)
}

type DocumentInfo struct {
	ID           int64
	Title        string
	Source       string
	Project      string
	Kind         string
	Status       string
	SupersededBy *int64
	Tags         []string
	Chunks       int
	UpdatedAt    time.Time
	Usage        Usage
}

type Document struct {
	DocumentInfo
	Content string
}

// SnapshotDoc is one entry with its text and its mean (document-level) embedding.
type SnapshotDoc struct {
	ID           int64
	Title        string
	Project      string
	Kind         string
	Status       string
	SupersededBy *int64
	Source       string
	Tags         []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Content      string
	Vec          []float32
	Usage        Usage
}

// Snapshot is a read-only view of the whole knowledge base for visualisation.
type Snapshot struct {
	Docs    []SnapshotDoc
	Total   int // entries in the database (may exceed len(Docs) when limited)
	Model   string
	DBBytes int64
}

// ReindexOptions configures Reindex.
type ReindexOptions struct {
	Model     string
	Dim       int
	DocPrefix string
	Batch     int
	// Embed returns one vector per text (same order).
	Embed func(ctx context.Context, texts []string) ([][]float32, error)
	Logf  func(format string, a ...any)
}
