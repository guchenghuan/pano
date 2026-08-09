// Command screenshot drives the pano binary in a pty through its canonical
// states and dumps the final screen of each as JSON (docs/screenshots/json),
// which render.py turns into PNGs for the README. Run from the repo root:
//
//	go run ./tools/screenshot && python3 tools/screenshot/render.py
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"pano/internal/vt10x"
)

const (
	cols = 112
	rows = 30
)

// session is a running pano under a pty with its raw output captured.
type session struct {
	cmd  *exec.Cmd
	ptmx *os.File
	mu   sync.Mutex
	out  []byte
}

func newSession() *session {
	bin := filepath.Join(os.TempDir(), "pano-shot-bin")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fatal("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin)
	// Run from a scratch dir named "pano" so pane titles show the product
	// name rather than whatever directory the tool runs in.
	workDir := filepath.Join(os.TempDir(), "pano-shot-work", "pano")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("workdir: %v", err)
	}
	cmd.Dir = workDir
	// Isolated config dir: no real agents.toml / session.json interference.
	// Wiped per run so a previous run's saved session never prompts.
	cfgDir := os.TempDir() + "/pano-shot-cfg"
	_ = os.RemoveAll(cfgDir)
	env := []string{
		"TERM=xterm-256color",
		"XDG_CONFIG_HOME=" + cfgDir,
		"BASH_SILENCE_DEPRECATION_WARNING=1",
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TERM=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") || strings.HasPrefix(kv, "NO_COLOR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		fatal("pty: %v", err)
	}
	s := &session{cmd: cmd, ptmx: ptmx}
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.out = append(s.out, buf[:n]...)
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	// Answer the terminal queries (bg color, cursor position) so the
	// renderer doesn't stall waiting for a real terminal's response.
	go func() {
		answeredOSC, answeredCPR := false, false
		for !answeredOSC || !answeredCPR {
			s.mu.Lock()
			str := string(s.out)
			s.mu.Unlock()
			if !answeredOSC && strings.Contains(str, "\x1b]11;?") {
				ptmx.Write([]byte("\x1b]11;rgb:1b1b/1b23/1b23\x1b\\"))
				answeredOSC = true
			}
			if !answeredCPR && strings.Contains(str, "\x1b[6n") {
				ptmx.Write([]byte("\x1b[1;1R"))
				answeredCPR = true
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return s
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "screenshot: "+format+"\n", args...)
	os.Exit(1)
}

func (s *session) contains(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(string(s.out), sub)
}

func (s *session) waitFor(what, sub string) {
	deadline := time.Now().Add(10 * time.Second)
	for !s.contains(sub) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !s.contains(sub) {
		fatal("%s: %q never appeared", what, sub)
	}
}

func (s *session) send(keys string) {
	if _, err := s.ptmx.Write([]byte(keys)); err != nil {
		fatal("send: %v", err)
	}
}

func (s *session) stop() {
	s.send("\x1b[20~") // F9
	s.waitFor("quit prompt", "quit all terminals?")
	s.send("y")
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	_ = s.ptmx.Close()
}

// cell is one terminal cell in the JSON dump.
type cell struct {
	C  string `json:"c"`
	Fg string `json:"fg"`
	Bg string `json:"bg"`
	B  bool   `json:"b,omitempty"` // bold
}

// dump replays the captured output into a fresh vt10x and writes the final
// screen as JSON.
func (s *session) dump(path string) {
	s.mu.Lock()
	data := make([]byte, len(s.out))
	copy(data, s.out)
	s.mu.Unlock()

	vt := vt10x.New(vt10x.WithSize(cols, rows))
	vt.Write(data)

	grid := make([][]cell, rows)
	for y := 0; y < rows; y++ {
		row := make([]cell, cols)
		for x := 0; x < cols; x++ {
			g := vt.Cell(x, y)
			ch := " "
			if g.Char != 0 {
				ch = string(g.Char)
			}
			row[x] = cell{C: ch, Fg: colorStr(g.FG, true), Bg: colorStr(g.BG, false), B: g.Mode&4 != 0}
		}
		grid[y] = row
	}
	doc := map[string]any{"cols": cols, "rows": rows, "cells": grid}
	out, err := json.Marshal(doc)
	if err != nil {
		fatal("json: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal(err.Error())
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fatal(err.Error())
	}
	fmt.Println("dumped", path)
}

// colorStr encodes a vt10x color: "def" for the terminal default, a decimal
// string for the 256 palette, "#rrggbb" for truecolor.
func colorStr(c vt10x.Color, fg bool) string {
	if c == vt10x.DefaultFG || c == vt10x.DefaultBG {
		return "def"
	}
	if c < 256 {
		return strconv.Itoa(int(c))
	}
	return fmt.Sprintf("#%06x", uint32(c))
}

// notifyViaCtl sends a notify command for paneID over the instance's
// control socket, exactly what `pano ctl notify` does from inside a pane.
func notifyViaCtl(s *session, paneID int, text string) {
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("pano-%d.sock", s.cmd.Process.Pid))
	c, err := net.Dial("unix", sock)
	if err != nil {
		fatal("ctl dial: %v", err)
	}
	fmt.Fprintf(c, "%d notify %s\n", paneID, text)
	c.Close()
}

func main() {
	outDir := filepath.Join("docs", "screenshots", "json")
	s := newSession()
	defer func() { _ = s.ptmx.Close(); _ = s.cmd.Process.Kill() }()
	s.waitFor("initial render", "even-grid")

	// stty -echo + set +m: typed commands and job notices stay out of the
	// shots; clear wipes the shell's startup banner.
	const quiet = "stty -echo\rset +m\rclear\r"

	// Pane 2 (focused at startup): a finished test run.
	s.send(quiet)
	s.send("printf 'PASS\\nok  \\tpano\\t7.3s\\n'\r")
	s.waitFor("test output", "7.3s")

	// Pane 1: an agent session at its prompt.
	s.send("\x1b1")
	s.send(quiet)
	s.send("printf '\\033[35m── Claude Code v2.1.215 ──\\033[0m\\n\\nWelcome back!\\n\\n\\342\\235\\257 \\n'\r")
	s.waitFor("agent screen", "Welcome back")

	// Third pane with some output in flight.
	s.send("\x1b[12~") // F2
	s.waitFor("third pane", "3 ·")
	s.send(quiet)
	s.send("printf 'rendering frames… 78%%\\n'\r")
	s.waitFor("pane 3 output", "78%")

	// A notification lands on pane 1 over the control socket while it is
	// unfocused: red border + red dot + a notes-drawer entry.
	notifyViaCtl(s, 1, "Build;Finished")
	time.Sleep(500 * time.Millisecond)
	s.dump(filepath.Join(outDir, "grid.json"))

	// Focus mode: the sidebar minis show each pane's live tail.
	s.send("\x1b[14~") // F4
	s.waitFor("focus mode", "focus")
	time.Sleep(500 * time.Millisecond)
	s.dump(filepath.Join(outDir, "focus.json"))

	// Notification history drawer.
	s.send("\x1b[19~") // F8
	s.waitFor("notes drawer", "notifications")
	time.Sleep(300 * time.Millisecond)
	s.dump(filepath.Join(outDir, "notes.json"))
	s.send("\x1b") // close the drawer
	time.Sleep(500 * time.Millisecond)

	s.stop()
}
