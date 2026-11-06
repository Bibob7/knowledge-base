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

// runSubcommand fuehrt Hilfs-Befehle aus. handled=false: normal als MCP-Server starten.
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "hook":
		// Ein Hook darf die Sitzung nie stoeren: immer Exit-Code 0.
		if err := runHook(); err != nil && os.Getenv("KB_HOOK_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "kb-hook: fehler:", err)
		}
		return true, 0
	case "extract":
		if err := runExtract(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, time.Now().Format(time.RFC3339), "extract fehler:", err)
			return true, 1
		}
		return true, 0
	case "install-hooks", "uninstall-hooks":
		if err := manageHooks(args[0] == "install-hooks"); err != nil {
			fmt.Fprintln(os.Stderr, "fehler:", err)
			return true, 1
		}
		return true, 0
	}
	return false, 0
}

func runHook() error {
	// Der Hintergrundlauf von 'claude -p' darf keine Hooks von uns ausloesen.
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
		fmt.Printf("%s: keine Aenderung noetig\n", path)
	case install:
		fmt.Printf("Hooks (%v) in %s eingetragen (Backup: %s.kb-backup)\n", hook.Events, path, path)
	default:
		fmt.Printf("Hooks aus %s entfernt (Backup: %s.kb-backup)\n", path, path)
	}
	return nil
}

// onSessionEnd startet die Auswertung im Hintergrund und kehrt sofort zurueck.
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

// runExtract wertet ein Transkript aus (vom Hook gestartet oder manuell: --dry-run).
func runExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	transcript := fs.String("transcript", "", "Pfad zum Transkript (JSONL)")
	session := fs.String("session", "", "Session-ID")
	cwd := fs.String("cwd", "", "Arbeitsverzeichnis der Sitzung (bestimmt das Projekt)")
	project := fs.String("project", "", "Projektname statt Ermittlung aus --cwd")
	dry := fs.Bool("dry-run", os.Getenv("KB_EXTRACT_DRY_RUN") != "", "nur anzeigen, nichts speichern")
	delay := fs.Duration("delay", 0, "Wartezeit vor dem Start (Transkript soll fertig geschrieben sein)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *transcript == "" || *session == "" {
		return fmt.Errorf("--transcript und --session sind Pflicht")
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
