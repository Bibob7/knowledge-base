package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sessions that never end cleanly (terminal closed, crash, kill) never fire SessionEnd,
// so their transcripts would never be evaluated. Sweep finds such transcripts and
// evaluates them later, e.g. when the next session starts.

// SweepOptions controls which transcripts Sweep evaluates.
type SweepOptions struct {
	Root           string        // projects directory with <project>/<session>.jsonl (default: ProjectsRoot())
	CurrentSession string        // the session that triggered the sweep is never evaluated
	Idle           time.Duration // a transcript must not have changed for this long (it may be live in another window)
	MaxAge         time.Duration // older transcripts are ignored
	Max            int           // evaluate at most this many transcripts per sweep (newest first)
	MinInterval    time.Duration // at most one sweep per interval
	Force          bool          // ignore MinInterval, Idle and the failure back-off
	DryRun         bool
	// ProjectOf maps a working directory to a project name (empty = cross-project).
	ProjectOf func(ctx context.Context, cwd string) string
	Now       func() time.Time
}

// Candidate is a transcript that still has to be evaluated.
type Candidate struct {
	Path    string
	Session string
	ModTime time.Time
}

const (
	sweepLockStale = 30 * time.Minute
	failBackoff    = time.Hour
)

// ProjectsRoot is the directory where Claude Code keeps the transcripts.
func ProjectsRoot() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Sweep evaluates transcripts of sessions that have not been evaluated and have been idle.
func (d Deps) Sweep(ctx context.Context, o SweepOptions) error {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Root == "" {
		o.Root = ProjectsRoot()
	}
	if o.Idle <= 0 {
		o.Idle = 30 * time.Minute
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 7 * 24 * time.Hour
	}
	if o.Max <= 0 {
		o.Max = 3
	}
	if o.MinInterval <= 0 {
		o.MinInterval = 10 * time.Minute
	}
	if d.StateDir == "" || o.Root == "" {
		return errors.New("sweep needs a state directory and a transcripts directory")
	}
	now := o.Now()

	release, ok := acquireLock(d.StateDir, "sweep", sweepLockStale, now)
	if !ok {
		d.Logf("sweep: another sweep is running")
		return nil
	}
	defer release()
	if !o.Force && !o.DryRun {
		if fi, err := os.Stat(filepath.Join(d.StateDir, "sweep.last")); err == nil && now.Sub(fi.ModTime()) < o.MinInterval {
			return nil // swept recently; stay quiet so frequent session starts cost nothing
		}
	}
	if !o.DryRun {
		touch(filepath.Join(d.StateDir, "sweep.last"), now)
	}

	cands, err := d.FindCandidates(o, now)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return nil
	}
	d.Logf("sweep: %d transcript(s) to evaluate", len(cands))
	for _, c := range cands {
		cwd := TranscriptCwd(c.Path)
		project := ""
		if o.ProjectOf != nil {
			project = o.ProjectOf(ctx, cwd)
		}
		d.Logf("sweep: evaluating session %s (last change %s, cwd %q)", c.Session, c.ModTime.Format(time.RFC3339), cwd)
		err := d.Run(ctx, Options{Transcript: c.Path, SessionID: c.Session, Project: project, DryRun: o.DryRun})
		if err != nil {
			d.Logf("sweep: session %s failed, will retry later: %v", c.Session, err)
			if !o.DryRun {
				touch(d.failPath(c.Session), now)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// FindCandidates lists transcripts that were not (completely) evaluated, are idle and
// recent enough, newest first, at most o.Max.
func (d Deps) FindCandidates(o SweepOptions, now time.Time) ([]Candidate, error) {
	// Transcripts older than the first sweep belong to the time before this feature
	// existed; only sessions from then on are picked up.
	since, err := d.sweepSince(now)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(o.Root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, f := range files {
		session := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if !safeID.MatchString(session) || session == o.CurrentSession {
			continue
		}
		fi, err := os.Stat(f)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		age := now.Sub(fi.ModTime())
		if age > o.MaxAge || fi.ModTime().Before(since) {
			continue
		}
		if !o.Force {
			if age < o.Idle {
				continue
			}
			if ff, err := os.Stat(d.failPath(session)); err == nil && now.Sub(ff.ModTime()) < failBackoff {
				continue
			}
		}
		// Already evaluated up to the last change of the transcript?
		if pf, err := os.Stat(d.progressPath(session)); err == nil && !pf.ModTime().Before(fi.ModTime()) {
			continue
		}
		out = append(out, Candidate{Path: f, Session: session, ModTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	if len(out) > o.Max {
		out = out[:o.Max]
	}
	return out, nil
}

// sweepSince returns the time of the first sweep (created on first use).
func (d Deps) sweepSince(now time.Time) (time.Time, error) {
	p := filepath.Join(d.StateDir, "sweep.since")
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime(), nil
	}
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return time.Time{}, err
	}
	if err := os.WriteFile(p, []byte(now.Format(time.RFC3339)), 0o600); err != nil {
		return time.Time{}, err
	}
	_ = os.Chtimes(p, now, now)
	return now, nil
}

func (d Deps) failPath(session string) string {
	return filepath.Join(d.StateDir, session+".fail")
}

// TranscriptCwd returns the working directory recorded in the first lines of a transcript.
func TranscriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<20)
	for i := 0; i < 300; i++ {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"cwd"`)) {
			var e struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &e) == nil && e.Cwd != "" {
				return e.Cwd
			}
		}
		if errors.Is(err, io.EOF) || err != nil {
			break
		}
	}
	return ""
}

// acquireLock creates <dir>/<name>.lock exclusively. A lock older than stale is taken over
// (its owner has probably crashed). The returned func releases the lock.
func acquireLock(dir, name string, stale time.Duration, now time.Time) (func(), bool) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}, false
	}
	p := filepath.Join(dir, name+".lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, true
		}
		fi, serr := os.Stat(p)
		if serr != nil || now.Sub(fi.ModTime()) < stale {
			break
		}
		os.Remove(p) // stale
	}
	return func() {}, false
}

func touch(path string, t time.Time) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return
	}
	_ = os.Chtimes(path, t, t)
}
