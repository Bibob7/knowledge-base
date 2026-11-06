//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Bibob7/knowledge-base/internal/extract"
	"github.com/Bibob7/knowledge-base/internal/viz"
)

func vizPidPath() string { return filepath.Join(extract.CacheDir(), "viz.pid") }
func vizLogPath() string { return filepath.Join(extract.CacheDir(), "viz.log") }

// vizRunning reports whether our visualisation answers on addr.
func vizRunning(addr string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/api/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return resp.StatusCode == 200 && string(b) == viz.PingBody
}

// detachViz starts the visualisation as an independent background process (or reuses a
// running one), waits until it answers and opens the browser.
func detachViz(addr string, limit int, idle time.Duration, open bool) error {
	url := "http://" + addr + "/"
	if vizRunning(addr) {
		fmt.Println("Knowledge base view is already running: " + url)
	} else {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(extract.CacheDir(), 0o700); err != nil {
			return err
		}
		logf, err := os.OpenFile(vizLogPath(), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer logf.Close()
		cmd := exec.Command(exe, "viz", "-addr", addr, "-limit", strconv.Itoa(limit), "-idle", idle.String())
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives the calling session
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		pid := cmd.Process.Pid
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		deadline := time.After(15 * time.Second)
	wait:
		for !vizRunning(addr) {
			select {
			case <-exited:
				return fmt.Errorf("the visualisation did not start (port %s in use?): %s", addr, logTail(vizLogPath()))
			case <-deadline:
				return fmt.Errorf("the visualisation did not answer within 15 s, see %s", vizLogPath())
			case <-time.After(150 * time.Millisecond):
				continue wait
			}
		}
		_ = os.WriteFile(vizPidPath(), []byte(strconv.Itoa(pid)), 0o600)
		fmt.Printf("Knowledge base view started in the background: %s (pid %d, stops %s after the last tab was closed)\n",
			url, pid, idle)
	}
	if open {
		openBrowser(url)
	}
	return nil
}

// stopViz stops the background visualisation started by detachViz.
func stopViz() error {
	b, err := os.ReadFile(vizPidPath())
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("No visualisation is running in the background.")
		return nil
	} else if err != nil {
		return err
	}
	defer os.Remove(vizPidPath())
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("%s: %w", vizPidPath(), err)
	}
	// Only signal the process if it still is our visualisation (the pid may have been reused).
	out, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if !strings.Contains(string(out), "knowledge-base-mcp") || !strings.Contains(string(out), " viz") {
		fmt.Println("No visualisation is running in the background.")
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	fmt.Printf("Visualisation stopped (pid %d).\n", pid)
	return nil
}

func logTail(path string) string {
	b, _ := os.ReadFile(path)
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return s
}
