package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kevin/knowledge-base/internal/config"
	"github.com/kevin/knowledge-base/internal/embed"
	"github.com/kevin/knowledge-base/internal/extract"
	"github.com/kevin/knowledge-base/internal/hook"
	"github.com/kevin/knowledge-base/internal/store"
	"github.com/kevin/knowledge-base/internal/viz"
)

// runSubcommand runs helper commands. handled=false: start normally as an MCP server.
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "hook":
		// A hook must never disturb the session: always exit code 0.
		if err := runHook(); err != nil && os.Getenv("KB_HOOK_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "kb-hook: error:", err)
		}
		return true, 0
	case "extract":
		if err := runExtract(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, time.Now().Format(time.RFC3339), "extract error:", err)
			return true, 1
		}
		return true, 0
	case "sweep":
		if err := runSweep(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, time.Now().Format(time.RFC3339), "sweep error:", err)
			return true, 1
		}
		return true, 0
	case "reindex":
		if err := runReindex(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return true, 1
		}
		return true, 0
	case "viz":
		if err := runViz(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return true, 1
		}
		return true, 0
	case "uninstall-hooks":
		// Hooks now come with the Claude Code plugin; this only cleans up hooks that older
		// versions wrote into ~/.claude/settings.json.
		if err := uninstallLegacyHooks(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return true, 1
		}
		return true, 0
	}
	return false, 0
}

func runHook() error {
	// The background run of 'claude -p' must not trigger any hooks of ours.
	if os.Getenv(extract.GuardEnv) != "" {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		return err
	}
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	var peek struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		Cwd            string `json:"cwd"`
		HookEventName  string `json:"hook_event_name"`
		Source         string `json:"source"`
	}
	if json.Unmarshal(data, &peek) == nil {
		switch peek.HookEventName {
		case "SessionEnd":
			return onSessionEnd(peek.TranscriptPath, peek.SessionID, peek.Cwd)
		case "SessionStart":
			// Silent: nothing is injected into the context. A background sweep evaluates
			// sessions that never ended cleanly. 'compact' happens mid-session, skip it.
			if peek.Source == "compact" {
				return nil
			}
			return onSessionStart(peek.SessionID)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		return err
	}
	defer st.Close()

	d := hook.Deps{
		Store:       st,
		Embed:       embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim),
		QueryPrefix: cfg.EmbeddingQueryPrefix,
		MinSim:      cfg.HookMinSim,
	}
	if os.Getenv("KB_HOOK_DEBUG") != "" {
		d.Debug = os.Stderr
	}
	return hook.Run(ctx, bytes.NewReader(data), os.Stdout, d)
}

func uninstallLegacyHooks() error {
	path, err := hook.DefaultSettingsPath()
	if err != nil {
		return err
	}
	changed, err := hook.Uninstall(path)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("Hooks removed from %s (backup: %s.kb-backup)\n", path, path)
	} else {
		fmt.Printf("%s: no change needed\n", path)
	}
	return nil
}

// autoLearnEnabled reports whether KB_AUTO_LEARN allows evaluating transcripts.
func autoLearnEnabled() bool {
	switch strings.ToLower(os.Getenv("KB_AUTO_LEARN")) {
	case "0", "off", "false", "no":
		return false
	}
	return true
}

// onSessionStart starts a sweep for unevaluated transcripts in the background and returns
// immediately. It prints nothing, so no knowledge is injected at session start.
func onSessionStart(session string) error {
	if !autoLearnEnabled() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return extract.SpawnSweep(exe, session)
}

// onSessionEnd starts the extraction in the background and returns immediately.
func onSessionEnd(transcript, session, cwd string) error {
	if !autoLearnEnabled() {
		return nil
	}
	if transcript == "" || session == "" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return extract.Spawn(exe, transcript, session, cwd)
}

// runExtract evaluates a transcript (started by the hook or manually: --dry-run).
func runExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	transcript := fs.String("transcript", "", "path to the transcript (JSONL)")
	session := fs.String("session", "", "Session-ID")
	cwd := fs.String("cwd", "", "working directory of the session (determines the project)")
	project := fs.String("project", "", "project name instead of deriving it from --cwd")
	dry := fs.Bool("dry-run", os.Getenv("KB_EXTRACT_DRY_RUN") != "", "only show, do not store")
	delay := fs.Duration("delay", 0, "wait before starting (the transcript should be fully written)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *transcript == "" || *session == "" {
		return fmt.Errorf("--transcript and --session are required")
	}
	time.Sleep(*delay)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	d, closeFn, err := newExtractDeps(ctx)
	if err != nil {
		return err
	}
	defer closeFn()

	proj := *project
	if proj == "" {
		proj = hook.ProjectFromDir(ctx, *cwd)
	}
	return d.Run(ctx, extract.Options{Transcript: *transcript, SessionID: *session, Project: proj, DryRun: *dry})
}

// newExtractDeps wires the store, embedding client and model runner for evaluating transcripts.
func newExtractDeps(ctx context.Context) (extract.Deps, func(), error) {
	if err := config.LoadDotEnv(); err != nil {
		return extract.Deps{}, nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return extract.Deps{}, nil, err
	}
	st, err := store.OpenReadOnly(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		return extract.Deps{}, nil, err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	return extract.Deps{
		Store:         st,
		Embed:         embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim),
		DocPrefix:     cfg.EmbeddingDocPrefix,
		ConflictFloor: cfg.SimilarMin,
		Runner:        extract.ClaudeCLI{Model: os.Getenv("KB_EXTRACT_MODEL")},
		StateDir:      extract.StateDir(),
		Logf:          func(f string, a ...any) { logger.Printf("extract: "+f, a...) },
	}, st.Close, nil
}

// runSweep evaluates transcripts of sessions that never ended cleanly (see extract.Sweep).
// It is started by the SessionStart hook; it can also be run by hand (--dry-run, --force).
func runSweep(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	current := fs.String("current-session", "", "session that triggered the sweep (never evaluated)")
	root := fs.String("root", "", "transcripts directory (default ~/.claude/projects)")
	idle := fs.Duration("idle", envDuration("KB_SWEEP_IDLE", 30*time.Minute), "a transcript must have been unchanged for this long")
	maxAge := fs.Duration("max-age", envDuration("KB_SWEEP_MAX_AGE", 7*24*time.Hour), "ignore transcripts older than this")
	maxN := fs.Int("max", 3, "evaluate at most this many transcripts per sweep")
	dry := fs.Bool("dry-run", false, "only show what would be stored")
	force := fs.Bool("force", false, "ignore the minimum interval, the idle time and the failure back-off")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !autoLearnEnabled() && !*force {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	d, closeFn, err := newExtractDeps(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	return d.Sweep(ctx, extract.SweepOptions{
		Root: *root, CurrentSession: *current, Idle: *idle, MaxAge: *maxAge, Max: *maxN,
		Force: *force, DryRun: *dry, ProjectOf: hook.ProjectFromDir,
	})
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

// runReindex re-embeds all stored chunks with the configured model (see store.Reindex).
func runReindex(args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.OpenReadOnly(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		return err
	}
	defer st.Close()
	dim, model, err := st.CurrentEmbedding(ctx)
	if err != nil {
		return err
	}
	if model == "" {
		model = "(unknown)"
	}
	fmt.Printf("stored vectors:  model %s, %d dimensions\nconfigured:      model %s, %d dimensions\n", model, dim, cfg.EmbeddingModel, cfg.EmbeddingDim)
	fmt.Println("All chunks will be re-embedded and the vector column and index rebuilt. Make a backup first (pg_dump).")
	if !*yes {
		fmt.Print("Continue? [y/N] ")
		var a string
		fmt.Scanln(&a)
		if a = strings.ToLower(strings.TrimSpace(a)); a != "y" && a != "yes" {
			return fmt.Errorf("aborted")
		}
	}
	client := embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim)
	return st.Reindex(ctx, store.ReindexOptions{
		Model:     cfg.EmbeddingModel,
		Dim:       cfg.EmbeddingDim,
		DocPrefix: cfg.EmbeddingDocPrefix,
		Embed:     client.Embed,
		Logf:      func(f string, a ...any) { fmt.Printf("reindex: "+f+"\n", a...) },
	})
}

// runViz serves a live, read-only view of the knowledge base on a loopback address.
func runViz(args []string) error {
	fs := flag.NewFlagSet("viz", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address (loopback only, there is no authentication)")
	open := fs.Bool("open", false, "open the page in the default browser")
	limit := fs.Int("limit", 5000, "show at most this many entries (newest first)")
	detach := fs.Bool("detach", false, "run in the background (reuses a running instance) and return")
	stopRunning := fs.Bool("stop", false, "stop the visualisation running in the background")
	idle := fs.Duration("idle", 0, "stop after this long without requests, e.g. after the tab was closed (0 = never; default with -detach: 30m)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case *stopRunning:
		return stopViz()
	case *detach:
		if *idle == 0 {
			*idle = 30 * time.Minute
		}
		return detachViz(*addr, *limit, *idle, *open)
	}
	if err := config.LoadDotEnv(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.OpenReadOnly(ctx, cfg.DatabaseURL, cfg.EmbeddingDim)
	if err != nil {
		return err
	}
	defer st.Close()
	return viz.Serve(ctx, *addr, viz.Options{Source: st, Limit: *limit, IdleTimeout: *idle}, func(a string) {
		url := "http://" + a + "/"
		fmt.Println("Knowledge base view: " + url + "  (Ctrl+C to stop)")
		if *open {
			openBrowser(url)
		}
	})
}

func openBrowser(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, url).Start()
}
