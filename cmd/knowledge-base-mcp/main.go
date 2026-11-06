// knowledge-base-mcp ist ein MCP-Server (stdio) fuer eine Wissensdatenbank auf
// Basis von Postgres/pgvector mit Hybrid-Suche (Vektor + Volltext).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kevin/knowledge-base/internal/chunk"
	"github.com/kevin/knowledge-base/internal/config"
	"github.com/kevin/knowledge-base/internal/embed"
	"github.com/kevin/knowledge-base/internal/secrets"
	"github.com/kevin/knowledge-base/internal/store"
)

type app struct {
	cfg   config.Config
	store *store.Store
	embed *embed.Client
}

func main() {
	// stdout gehoert dem MCP-Protokoll, Logs gehen nach stderr.
	log.SetOutput(os.Stderr)
	log.SetPrefix("knowledge-base-mcp: ")

	if handled, code := runSubcommand(os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := config.LoadDotEnv(); err != nil {
		return fmt.Errorf(".env: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		return err
	}
	defer st.Close()

	a := &app{
		cfg:   cfg,
		store: st,
		embed: embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim),
	}

	if len(os.Args) > 1 && os.Args[1] == "--check" {
		return a.check(ctx)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "knowledge-base", Version: "0.2.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "remember",
		Description: "Merkt sich einen kurzen, atomaren Fakt (1-5 Saetze): Korrekturen des Nutzers, Entscheidungen samt Begruendung, " +
			"Projektkonventionen, Praeferenzen, verifizierte Erkenntnisse. Ein Fakt pro Aufruf. " +
			"Die Antwort enthaelt aehnliche bestehende Eintraege ('similar'). Widerspricht einer davon dem neuen Fakt oder ist " +
			"er veraltet, ruf supersede auf (oder uebergib beim naechsten remember 'supersedes'), damit nichts Falsches stehen bleibt.",
	}, a.remember)

	mcp.AddTool(server, &mcp.Tool{
		Name: "supersede",
		Description: "Markiert bestehende Eintraege als veraltet, weil sie durch einen neuen Eintrag ersetzt wurden " +
			"(Korrektur). Veraltete Eintraege werden nicht mehr gefunden, bleiben aber als Historie erhalten.",
	}, a.supersede)

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_document",
		Description: "Speichert ein laengeres Dokument (wird gechunkt und eingebettet). Fuer kurze Fakten stattdessen remember nutzen. " +
			"Ist 'source' gesetzt und existiert bereits ein aktives Dokument mit derselben source, wird es ersetzt.",
	}, a.addDocument)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_knowledge",
		Description: "Durchsucht die Knowledge Base per Hybrid-Suche (semantisch + Stichwort) nach aktuellem Wissen. " +
			"Mit 'project' werden Eintraege dieses Projekts plus projektuebergreifende gefunden.",
	}, a.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_documents",
		Description: "Listet Eintraege (zuletzt geaenderte zuerst), optional nach Projekt/Tag gefiltert. Zeigt, was zuletzt gelernt wurde.",
	}, a.listDocuments)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_document",
		Description: "Liefert den Inhalt eines Eintrags anhand seiner ID (auch veraltete).",
	}, a.getDocument)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_document",
		Description: "Loescht einen Eintrag endgueltig samt Chunks. Fuer Korrekturen stattdessen supersede nutzen; loeschen nur bei Unsinn oder auf Wunsch.",
	}, a.deleteDocument)

	log.Printf("bereit (modell=%s, dim=%d)", cfg.EmbeddingModel, cfg.EmbeddingDim)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

const (
	// nomic-embed-text hat ein hohes Grundrauschen (~0.7 bei themenfremden Saetzen).
	similarThreshold = 0.78
	similarLimit     = 4
)

func (a *app) embedDocs(ctx context.Context, content string) ([]store.ChunkInput, error) {
	parts := chunk.Split(content, a.cfg.ChunkSize, a.cfg.ChunkOverlap)
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = a.cfg.EmbeddingDocPrefix + p
	}
	vecs, err := a.embed.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	chunks := make([]store.ChunkInput, len(parts))
	for i := range parts {
		chunks[i] = store.ChunkInput{Content: parts[i], Embedding: vecs[i]}
	}
	return chunks, nil
}

// check prueft Datenbank und Embedding-Modell und beendet sich (fuer Installer/Fehlersuche).
func (a *app) check(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "datenbank: ok (schema bereit, dim=%d)\n", a.cfg.EmbeddingDim)
	if _, err := a.embed.Embed(ctx, []string{"healthcheck"}); err != nil {
		return fmt.Errorf("embedding (%s @ %s): %w", a.cfg.EmbeddingModel, a.cfg.EmbeddingBaseURL, err)
	}
	fmt.Fprintf(os.Stderr, "embedding: ok (%s)\n", a.cfg.EmbeddingModel)
	return nil
}

// --- remember ---

type rememberIn struct {
	Content    string   `json:"content" jsonschema:"Der Fakt als eigenstaendiger Text (1-5 Saetze), auch ohne Kontext verstaendlich; Begruendung (Warum) mitschreiben"`
	Title      string   `json:"title,omitempty" jsonschema:"Optionaler kurzer Titel; sonst wird er aus dem Text abgeleitet"`
	Project    string   `json:"project,omitempty" jsonschema:"Projektname (Name des Git-Repos); leer = projektuebergreifendes Wissen (Praeferenzen, Allgemeines)"`
	Tags       []string `json:"tags,omitempty" jsonschema:"Optionale Tags"`
	Supersedes []int64  `json:"supersedes,omitempty" jsonschema:"IDs bestehender Eintraege, die durch diesen Fakt ersetzt/korrigiert werden"`
}

type similarOut struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	Project    string  `json:"project,omitempty"`
	Similarity float64 `json:"similarity"`
	Content    string  `json:"content"`
}

type rememberOut struct {
	ID         int64        `json:"id"`
	Superseded []int64      `json:"superseded"`
	Similar    []similarOut `json:"similar"`
	Hint       string       `json:"hint,omitempty"`
}

func (a *app) remember(ctx context.Context, _ *mcp.CallToolRequest, in rememberIn) (*mcp.CallToolResult, rememberOut, error) {
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, rememberOut{}, errors.New("content darf nicht leer sein")
	}
	if err := secrets.Check(in.Title + "\n" + content); err != nil {
		return nil, rememberOut{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = deriveTitle(content)
	}
	chunks, err := a.embedDocs(ctx, content)
	if err != nil {
		return nil, rememberOut{}, err
	}
	// Aehnliche Eintraege vor dem Speichern ermitteln, damit der neue nicht sich selbst findet.
	cands, err := a.store.Similar(ctx, chunks[0].Embedding, in.Project, in.Supersedes, similarLimit)
	if err != nil {
		return nil, rememberOut{}, err
	}
	res, err := a.store.AddDocument(ctx, store.DocumentInput{
		Title: title, Project: in.Project, Kind: "fact", Tags: in.Tags,
		Supersedes: in.Supersedes, Chunks: chunks,
	})
	if err != nil {
		return nil, rememberOut{}, err
	}
	out := rememberOut{ID: res.ID, Superseded: nonNilIDs(res.Superseded), Similar: []similarOut{}}
	for _, c := range cands {
		if c.Similarity < similarThreshold {
			continue
		}
		out.Similar = append(out.Similar, similarOut{c.ID, c.Title, c.Project, round3(c.Similarity), snippet(c.Content, 300)})
	}
	if len(out.Similar) > 0 {
		out.Hint = "Aehnliche Eintraege gefunden. Widerspricht oder veraltet einer davon durch den neuen Fakt, " +
			"rufe supersede(old_ids=[...], new_id=" + strconv.FormatInt(res.ID, 10) + ") auf."
	}
	return nil, out, nil
}

// --- supersede ---

type supersedeIn struct {
	OldIDs []int64 `json:"old_ids" jsonschema:"IDs der veralteten Eintraege"`
	NewID  int64   `json:"new_id" jsonschema:"ID des Eintrags, der sie ersetzt (muss aktiv sein)"`
}

type supersedeOut struct {
	Superseded []int64 `json:"superseded"`
}

func (a *app) supersede(ctx context.Context, _ *mcp.CallToolRequest, in supersedeIn) (*mcp.CallToolResult, supersedeOut, error) {
	if len(in.OldIDs) == 0 {
		return nil, supersedeOut{}, errors.New("old_ids darf nicht leer sein")
	}
	ids, err := a.store.Supersede(ctx, in.OldIDs, in.NewID)
	if err != nil {
		return nil, supersedeOut{}, err
	}
	return nil, supersedeOut{Superseded: nonNilIDs(ids)}, nil
}

// --- add_document ---

type addDocumentIn struct {
	Title   string   `json:"title" jsonschema:"Titel des Dokuments"`
	Content string   `json:"content" jsonschema:"Volltext des Dokuments (Markdown oder Text)"`
	Source  string   `json:"source,omitempty" jsonschema:"Optionale eindeutige Quelle, z.B. URL oder Dateipfad; dient zum Ersetzen bei erneutem Einlesen"`
	Project string   `json:"project,omitempty" jsonschema:"Projektname; leer = projektuebergreifend"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optionale Tags zum Filtern"`
}

type addDocumentOut struct {
	ID       int64 `json:"id"`
	Chunks   int   `json:"chunks"`
	Replaced bool  `json:"replaced"`
}

func (a *app) addDocument(ctx context.Context, _ *mcp.CallToolRequest, in addDocumentIn) (*mcp.CallToolResult, addDocumentOut, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "" {
		return nil, addDocumentOut{}, errors.New("title und content duerfen nicht leer sein")
	}
	if err := secrets.Check(in.Title + "\n" + in.Content); err != nil {
		return nil, addDocumentOut{}, err
	}
	chunks, err := a.embedDocs(ctx, in.Content)
	if err != nil {
		return nil, addDocumentOut{}, err
	}
	res, err := a.store.AddDocument(ctx, store.DocumentInput{
		Title: in.Title, Source: in.Source, Project: in.Project, Kind: "document", Tags: in.Tags, Chunks: chunks,
	})
	if err != nil {
		return nil, addDocumentOut{}, err
	}
	return nil, addDocumentOut{ID: res.ID, Chunks: len(chunks), Replaced: res.Replaced}, nil
}

// --- search_knowledge ---

type searchIn struct {
	Query   string   `json:"query" jsonschema:"Suchanfrage in natuerlicher Sprache"`
	Project string   `json:"project,omitempty" jsonschema:"Projektname: findet dieses Projekt plus projektuebergreifendes Wissen; leer = alles"`
	Limit   int      `json:"limit,omitempty" jsonschema:"Maximale Trefferzahl (Standard 8, max 50)"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Nur Eintraege mit mindestens einem dieser Tags"`
}

type searchHit struct {
	DocumentID int64    `json:"document_id"`
	Title      string   `json:"title"`
	Project    string   `json:"project,omitempty"`
	Kind       string   `json:"kind"`
	Source     string   `json:"source,omitempty"`
	Tags       []string `json:"tags"`
	ChunkIndex int      `json:"chunk_index"`
	Score      float64  `json:"score"`
	Content    string   `json:"content"`
}

type searchOut struct {
	Hits []searchHit `json:"hits"`
}

func (a *app) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, searchOut{}, errors.New("query darf nicht leer sein")
	}
	limit := clamp(in.Limit, 8, 50)
	vecs, err := a.embed.Embed(ctx, []string{a.cfg.EmbeddingQueryPrefix + in.Query})
	if err != nil {
		return nil, searchOut{}, err
	}
	hits, err := a.store.Search(ctx, in.Query, vecs[0], in.Project, in.Tags, limit)
	if err != nil {
		return nil, searchOut{}, err
	}
	out := searchOut{Hits: make([]searchHit, len(hits))}
	for i, h := range hits {
		out.Hits[i] = searchHit{h.DocumentID, h.Title, h.Project, h.Kind, h.Source, nonNil(h.Tags), h.ChunkIndex, round3(h.Score), h.Content}
	}
	return nil, out, nil
}

// --- list_documents ---

type listIn struct {
	Project           string `json:"project,omitempty" jsonschema:"Projekt plus projektuebergreifende Eintraege; leer = alle"`
	Tag               string `json:"tag,omitempty" jsonschema:"Optionaler Tag-Filter"`
	IncludeSuperseded bool   `json:"include_superseded,omitempty" jsonschema:"Auch veraltete Eintraege zeigen"`
	Limit             int    `json:"limit,omitempty" jsonschema:"Maximale Anzahl (Standard 50, max 200)"`
}

type docInfo struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Project      string   `json:"project,omitempty"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	SupersededBy *int64   `json:"superseded_by,omitempty"`
	Source       string   `json:"source,omitempty"`
	Tags         []string `json:"tags"`
	Chunks       int      `json:"chunks"`
	UpdatedAt    string   `json:"updated_at"`
}

type listOut struct {
	Documents []docInfo `json:"documents"`
}

func toDocInfo(d store.DocumentInfo) docInfo {
	return docInfo{d.ID, d.Title, d.Project, d.Kind, d.Status, d.SupersededBy, d.Source, nonNil(d.Tags), d.Chunks,
		d.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")}
}

func (a *app) listDocuments(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	docs, err := a.store.ListDocuments(ctx, in.Tag, in.Project, in.IncludeSuperseded, clamp(in.Limit, 50, 200))
	if err != nil {
		return nil, listOut{}, err
	}
	out := listOut{Documents: make([]docInfo, len(docs))}
	for i, d := range docs {
		out.Documents[i] = toDocInfo(d)
	}
	return nil, out, nil
}

// --- get_document ---

type idIn struct {
	ID int64 `json:"id" jsonschema:"Dokument-ID"`
}

type getOut struct {
	docInfo
	Content string `json:"content"`
}

func (a *app) getDocument(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, getOut, error) {
	d, err := a.store.GetDocument(ctx, in.ID)
	if err != nil {
		return nil, getOut{}, err
	}
	return nil, getOut{docInfo: toDocInfo(d.DocumentInfo), Content: d.Content}, nil
}

// --- delete_document ---

type deleteOut struct {
	Deleted bool `json:"deleted"`
}

func (a *app) deleteDocument(ctx context.Context, _ *mcp.CallToolRequest, in idIn) (*mcp.CallToolResult, deleteOut, error) {
	if err := a.store.DeleteDocument(ctx, in.ID); err != nil {
		return nil, deleteOut{}, fmt.Errorf("delete_document %d: %w", in.ID, err)
	}
	return nil, deleteOut{Deleted: true}, nil
}

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}

func nonNilIDs(s []int64) []int64 {
	if s == nil {
		return []int64{}
	}
	return s
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func deriveTitle(content string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	return snippet(first, 80)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
