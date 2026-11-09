// knowledge-base-mcp is an MCP server (stdio) for a knowledge base built on
// a database (see internal/store) with hybrid search (vector + full text).
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Bibob7/knowledge-base/internal/chunk"
	"github.com/Bibob7/knowledge-base/internal/config"
	"github.com/Bibob7/knowledge-base/internal/embed"
	"github.com/Bibob7/knowledge-base/internal/project"
	"github.com/Bibob7/knowledge-base/internal/secrets"
	"github.com/Bibob7/knowledge-base/internal/store"
)

// instructions tell Claude when to search, remember and supersede. MCP clients put them into
// the system prompt (Claude Code cuts them after 4096 characters).
//
//go:embed instructions.md
var instructions string

// sessionProject is the project of the session that started this server: Claude Code
// starts it in the session's working directory. KB_PROJECT overrides it, as for the hook.
func sessionProject(ctx context.Context) string {
	if p := strings.ToLower(strings.TrimSpace(os.Getenv("KB_PROJECT"))); p != "" {
		return p
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return project.FromDir(ctx, wd)
}

// sessionInstructions names the project resolved by the server, so Claude uses exactly
// the name the hooks use instead of deriving it (worktrees, override file).
func sessionInstructions(proj string) string {
	if proj == "" {
		return instructions
	}
	return instructions + "\nTHIS SESSION'S PROJECT: \"" + proj + "\" (resolved by the server from the session's directory, " +
		"as the hooks do). Use exactly this name as project; derive it yourself only if you work in another repository.\n"
}

// version is set at build time (-ldflags "-X main.version=...") by scripts/kb.
var version = "dev"

type app struct {
	cfg   config.Config
	store store.Store
	embed *embed.Client
}

func main() {
	// stdout belongs to the MCP protocol, logs go to stderr.
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
	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.EmbeddingDim, cfg.EmbeddingModel)
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

	server := mcp.NewServer(&mcp.Implementation{Name: "knowledge-base", Version: version},
		&mcp.ServerOptions{Instructions: sessionInstructions(sessionProject(ctx))})

	mcp.AddTool(server, &mcp.Tool{
		Name: "remember",
		Description: "Remembers a short, atomic fact (1-5 sentences): corrections by the user, decisions including their reasoning, " +
			"project conventions, preferences, verified findings. One fact per call. " +
			"The response contains similar existing entries ('similar'). If one of them contradicts the new fact or is " +
			"outdated, call supersede (or pass 'supersedes' to remember), so that nothing wrong is left behind.",
	}, a.remember)

	mcp.AddTool(server, &mcp.Tool{
		Name: "supersede",
		Description: "Marks existing entries as outdated because a new entry replaces them (correction). " +
			"Outdated entries are no longer found by search but are kept as history.",
	}, a.supersede)

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_document",
		Description: "Stores a longer document (it is chunked and embedded). For short facts use remember instead. " +
			"If 'source' is set and an active document with the same source already exists, it is replaced.",
	}, a.addDocument)

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_knowledge",
		Description: "Searches the knowledge base for current knowledge using hybrid search (semantic + keyword). " +
			"With 'project', entries of that project plus cross-project entries are found.",
	}, a.search)

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_documents",
		Description: "Lists entries (most recently changed first), optionally filtered by project/tag. Shows what was learned recently. " +
			"Each entry shows how often it was injected or retrieved; with unused_days it lists entries nobody used for that long.",
	}, a.listDocuments)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_document",
		Description: "Returns the content of an entry by its ID (including outdated ones).",
	}, a.getDocument)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_document",
		Description: "Permanently deletes an entry including its chunks. Entries it had superseded become current again (or point to its own successor). For corrections use supersede instead; delete only junk or on request.",
	}, a.deleteDocument)

	log.Printf("ready (model=%s, dim=%d)", cfg.EmbeddingModel, cfg.EmbeddingDim)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// similarLimit is the maximum number of similar entries returned by remember.
const similarLimit = 4

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

// check verifies the database and the embedding model and exits (for the installer/troubleshooting).
func (a *app) check(ctx context.Context) error {
	fmt.Fprintf(os.Stderr, "database: ok (schema ready, dim=%d)\n", a.cfg.EmbeddingDim)
	if _, err := a.embed.Embed(ctx, []string{"healthcheck"}); err != nil {
		return fmt.Errorf("embedding (%s @ %s): %w", a.cfg.EmbeddingModel, a.cfg.EmbeddingBaseURL, err)
	}
	fmt.Fprintf(os.Stderr, "embedding: ok (%s)\n", a.cfg.EmbeddingModel)
	return nil
}

// --- remember ---

type rememberIn struct {
	Content    string   `json:"content" jsonschema:"The fact as self-contained text (1-5 sentences), understandable without context; include the reasoning (why)"`
	Title      string   `json:"title,omitempty" jsonschema:"Optional short title; otherwise derived from the text"`
	Project    string   `json:"project,omitempty" jsonschema:"Project name (name of the git repo); empty = cross-project knowledge (preferences, general facts)"`
	Tags       []string `json:"tags,omitempty" jsonschema:"Optional tags"`
	Supersedes []int64  `json:"supersedes,omitempty" jsonschema:"IDs of existing entries that this fact replaces/corrects"`
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
		return nil, rememberOut{}, errors.New("content must not be empty")
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
	// Determine similar entries before storing, so the new entry does not find itself.
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
		if c.Similarity < a.cfg.SimilarMin {
			continue
		}
		out.Similar = append(out.Similar, similarOut{c.ID, c.Title, c.Project, round3(c.Similarity), snippet(c.Content, 300)})
	}
	if len(out.Similar) > 0 {
		out.Hint = "Similar entries found. If one of them is contradicted or made outdated by the new fact, " +
			"call supersede(old_ids=[...], new_id=" + strconv.FormatInt(res.ID, 10) + ")."
	}
	return nil, out, nil
}

// --- supersede ---

type supersedeIn struct {
	OldIDs []int64 `json:"old_ids" jsonschema:"IDs of the outdated entries"`
	NewID  int64   `json:"new_id" jsonschema:"ID of the entry that replaces them (must be active)"`
}

type supersedeOut struct {
	Superseded []int64 `json:"superseded"`
}

func (a *app) supersede(ctx context.Context, _ *mcp.CallToolRequest, in supersedeIn) (*mcp.CallToolResult, supersedeOut, error) {
	if len(in.OldIDs) == 0 {
		return nil, supersedeOut{}, errors.New("old_ids must not be empty")
	}
	ids, err := a.store.Supersede(ctx, in.OldIDs, in.NewID)
	if err != nil {
		return nil, supersedeOut{}, err
	}
	return nil, supersedeOut{Superseded: nonNilIDs(ids)}, nil
}

// --- add_document ---

type addDocumentIn struct {
	Title   string   `json:"title" jsonschema:"Title of the document"`
	Content string   `json:"content" jsonschema:"Full text of the document (Markdown or plain text)"`
	Source  string   `json:"source,omitempty" jsonschema:"Optional unique source, e.g. URL or file path; used to replace the document when it is ingested again"`
	Project string   `json:"project,omitempty" jsonschema:"Project name; empty = cross-project"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional tags for filtering"`
}

type addDocumentOut struct {
	ID       int64 `json:"id"`
	Chunks   int   `json:"chunks"`
	Replaced bool  `json:"replaced"`
}

func (a *app) addDocument(ctx context.Context, _ *mcp.CallToolRequest, in addDocumentIn) (*mcp.CallToolResult, addDocumentOut, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "" {
		return nil, addDocumentOut{}, errors.New("title and content must not be empty")
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
	Query   string   `json:"query" jsonschema:"Search query in natural language"`
	Project string   `json:"project,omitempty" jsonschema:"Project name: finds this project plus cross-project knowledge; empty = everything"`
	Limit   int      `json:"limit,omitempty" jsonschema:"Maximum number of hits (default 8, max 50)"`
	Tags    []string `json:"tags,omitempty" jsonschema:"Only entries with at least one of these tags"`
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
		return nil, searchOut{}, errors.New("query must not be empty")
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
	ids := make([]int64, len(hits))
	for i, h := range hits {
		out.Hits[i] = searchHit{h.DocumentID, h.Title, h.Project, h.Kind, h.Source, nonNil(h.Tags), h.ChunkIndex, round3(h.Score), h.Content}
		ids[i] = h.DocumentID
	}
	a.recordUse(ctx, ids)
	return nil, out, nil
}

// --- list_documents ---

type listIn struct {
	Project           string `json:"project,omitempty" jsonschema:"Project plus cross-project entries; empty = all"`
	Tag               string `json:"tag,omitempty" jsonschema:"Optional tag filter"`
	IncludeSuperseded bool   `json:"include_superseded,omitempty" jsonschema:"Also show outdated entries"`
	UnusedDays        int    `json:"unused_days,omitempty" jsonschema:"Only active entries that were neither injected nor retrieved in the last N days (least recently used first), e.g. to review stale knowledge; tag is ignored"`
	Limit             int    `json:"limit,omitempty" jsonschema:"Maximum number (default 50, max 200)"`
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
	Injected     int      `json:"injected"`
	Retrieved    int      `json:"retrieved"`
	LastUsed     string   `json:"last_used,omitempty"`
}

type listOut struct {
	Documents []docInfo `json:"documents"`
	Note      string    `json:"note,omitempty"`
}

const timeFormat = "2006-01-02 15:04:05Z07:00"

func toDocInfo(d store.DocumentInfo) docInfo {
	out := docInfo{ID: d.ID, Title: d.Title, Project: d.Project, Kind: d.Kind, Status: d.Status,
		SupersededBy: d.SupersededBy, Source: d.Source, Tags: nonNil(d.Tags), Chunks: d.Chunks,
		UpdatedAt: d.UpdatedAt.Format(timeFormat), Injected: d.Usage.Injected, Retrieved: d.Usage.Retrieved}
	if d.Usage.LastUsed != nil {
		out.LastUsed = d.Usage.LastUsed.Format(timeFormat)
	}
	return out
}

// recordUse counts entries Claude retrieved. A failure is only logged: the counters are
// a hint for reviews and must never cost a result.
func (a *app) recordUse(ctx context.Context, ids []int64) {
	if err := a.store.RecordUse(ctx, ids, store.UseRetrieved); err != nil {
		log.Printf("recording usage: %v", err)
	}
}

func (a *app) listDocuments(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, listOut, error) {
	limit := clamp(in.Limit, 50, 200)
	var docs []store.DocumentInfo
	var note string
	var err error
	if in.UnusedDays > 0 {
		docs, note, err = a.unused(ctx, in.Project, in.UnusedDays, limit)
	} else {
		docs, err = a.store.ListDocuments(ctx, in.Tag, in.Project, in.IncludeSuperseded, limit)
	}
	if err != nil {
		return nil, listOut{}, err
	}
	out := listOut{Documents: make([]docInfo, len(docs)), Note: note}
	for i, d := range docs {
		out.Documents[i] = toDocInfo(d)
	}
	return nil, out, nil
}

// unused lists entries not used for `days` days. Usage is only known since tracking
// started; before that has run for `days` days, nothing can be called unused.
func (a *app) unused(ctx context.Context, project string, days, limit int) ([]store.DocumentInfo, string, error) {
	before := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	since, err := a.store.UsageSince(ctx)
	if err != nil {
		return nil, "", err
	}
	if since.IsZero() || since.After(before) {
		return []store.DocumentInfo{}, fmt.Sprintf("Usage is tracked since %s; entries can only be called unused for %d days "+
			"once tracking has run that long.", since.Format("2006-01-02"), days), nil
	}
	docs, err := a.store.Unused(ctx, project, before, limit)
	if err != nil {
		return nil, "", err
	}
	return docs, "Entries that were neither injected nor retrieved in the last " + strconv.Itoa(days) +
		" days. Unused is only a hint: review them, supersede what is outdated, keep what still holds.", nil
}

// --- get_document ---

type idIn struct {
	ID int64 `json:"id" jsonschema:"Document ID"`
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
	a.recordUse(ctx, []int64{d.ID})
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
