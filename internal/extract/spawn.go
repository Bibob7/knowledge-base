//go:build !windows

package extract

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// CacheDir ist das Verzeichnis fuer Log und Fortschrittsdateien.
func CacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "knowledge-base")
}

// LogPath ist die Logdatei der Hintergrundauswertung.
func LogPath() string { return filepath.Join(CacheDir(), "extract.log") }

// StateDir enthaelt die Fortschrittsdateien je Session.
func StateDir() string { return filepath.Join(CacheDir(), "state") }

// Spawn startet die Auswertung als eigenstaendigen Hintergrundprozess und kehrt
// sofort zurueck. Der Prozess ueberlebt das Ende der aufrufenden Sitzung.
func Spawn(exe, transcript, session, cwd string) error {
	if err := os.MkdirAll(CacheDir(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "extract",
		"--transcript", transcript, "--session", session, "--cwd", cwd, "--delay", "2s")
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return cmd.Process.Release()
}
