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
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kevin/knowledge-base/internal/config"
	"github.com/kevin/knowledge-base/internal/embed"
	"github.com/kevin/knowledge-base/internal/extract"
	"github.com/kevin/knowledge-base/internal/hook"
	"github.com/kevin/knowledge-base/internal/store"
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
	case "install-hooks", "uninstall-hooks":
		if err := manageHooks(args[0] == "install-hooks"); err != nil {
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
	}
	if json.Unmarshal(data, &peek) == nil && peek.HookEventName == "SessionEnd" {
		return onSessionEnd(peek.TranscriptPath, peek.SessionID, peek.Cwd)
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

	minSim := hook.DefaultMinSim
	if v := os.Getenv("KB_HOOK_MIN_SIM"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			minSim = f
		}
	}
	d := hook.Deps{
		Store:       st,
		Embed:       embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim),
		QueryPrefix: cfg.EmbeddingQueryPrefix,
		MinSim:      minSim,
	}
	if os.Getenv("KB_HOOK_DEBUG") != "" {
		d.Debug = os.Stderr
	}
	return hook.Run(ctx, bytes.NewReader(data), os.Stdout, d)
}

func manageHooks(install bool) error {
	path, err := hook.DefaultSettingsPath()
	if err != nil {
		return err
	}
	var changed bool
	if install {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		changed, err = hook.Install(path, hook.Command(exe))
		if err != nil {
			return err
		}
	} else if changed, err = hook.Uninstall(path); err != nil {
		return err
	}
	switch {
	case !changed:
		fmt.Printf("%s: no change needed\n", path)
	case install:
		fmt.Printf("Hooks (%v) added to %s (backup: %s.kb-backup)\n", hook.Events, path, path)
	default:
		fmt.Printf("Hooks removed from %s (backup: %s.kb-backup)\n", path, path)
	}
	return nil
}

// onSessionEnd starts the extraction in the background and returns immediately.
func onSessionEnd(transcript, session, cwd string) error {
	switch strings.ToLower(os.Getenv("KB_AUTO_LEARN")) {
	case "0", "off", "false", "no":
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

	proj := *project
	if proj == "" {
		proj = hook.ProjectFromDir(ctx, *cwd)
	}
	logger := log.New(os.Stderr, "", log.LstdFlags)
	d := extract.Deps{
		Store:     st,
		Embed:     embed.New(cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingAPIKey, cfg.EmbeddingDim),
		DocPrefix: cfg.EmbeddingDocPrefix,
		Runner:    extract.ClaudeCLI{Model: os.Getenv("KB_EXTRACT_MODEL")},
		StateDir:  extract.StateDir(),
		Logf:      func(f string, a ...any) { logger.Printf("extract: "+f, a...) },
	}
	return extract.Run(ctx, d, extract.Options{Transcript: *transcript, SessionID: *session, Project: proj, DryRun: *dry})
}
