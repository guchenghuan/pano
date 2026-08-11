package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"pano/internal/vt10x"
)

// scrollbackCap is the maximum number of history lines kept per pane.
const scrollbackCap = 1000

// attentionLevel ranks a pane's attention state for display and F7 jumps.
type attentionLevel int

const (
	attnIdle    attentionLevel = iota // dark grey dot
	attnActive                       // green dot: output within ~2s / agent working
	attnUnread                       // yellow dot+frame: unread output / agent blocked
	attnNotify                       // red dot+frame: BEL or OSC 9/99/777 notification
)

// activeWindow is how recent output must be to count as active.
const activeWindow = 2 * time.Second

// agentScreenLines is how many trailing screen lines the agent matcher scans.
const agentScreenLines = 6

// Pane is a single terminal pane: a real shell on a pty, a vt10x virtual
// terminal holding the screen buffer, a scrollback ring, and an editable
// title.
type Pane struct {
	id    int
	title string

	cmd  *exec.Cmd
	ptmx *os.File
	out  io.Writer // where key bytes go; ptmx in production, recorder in tests
	vt   vt10x.Terminal

	w, h int // content size in cells (pty + vt size)

	scrollback [][]vt10x.Glyph // lines scrolled off the top, oldest first
	scroll     int             // lines the view is scrolled up; 0 = live

	// attention/meta state, guarded by stateMu (reader goroutine writes,
	// Update/render read)
	stateMu    sync.Mutex
	lastOutput time.Time
	unread     bool   // output while unfocused
	notified   bool   // BEL or OSC 9/99/777 received
	oscNote    string // latest OSC notification payload (truncated)
	notes      []noteEvent // notification history ring (oldest first)
	agent      string // foreground agent name ("" if none), set by meta tick
	proc       string // foreground process name, set by meta tick
	branch     string // git branch of the pane's cwd, set by meta tick
	pid        int    // shell process pid

	refresh chan struct{} // shared, cap 1: coalesced redraw notification
	exited  chan *Pane    // shared: shell process exit notification
}

func shellPath() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/zsh"
}

// defaultTitle derives a title from the pane's working directory (its base
// name), falling back to the shell name.
func defaultTitle(dir string) string {
	if b := filepath.Base(dir); b != "." && b != "/" && b != "" {
		return b
	}
	return filepath.Base(shellPath())
}

func newPane(id int, w, h int, dir string, refresh chan struct{}, exited chan *Pane) (*Pane, error) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	cmd := exec.Command(shellPath())
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.Env = append(cmd.Env, paneCtlEnv(id)...)
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
	if err != nil {
		return nil, err
	}
	vt := vt10x.New(vt10x.WithSize(w, h))
	p := &Pane{
		id:      id,
		title:   defaultTitle(dir),
		cmd:     cmd,
		ptmx:    ptmx,
		out:     ptmx,
		vt:      vt,
		w:       w,
		h:       h,
		refresh: refresh,
		exited:  exited,
		pid:     cmd.Process.Pid,
	}
	if s, ok := vt.(interface{ SetScrollHook(func([][]vt10x.Glyph)) }); ok {
		s.SetScrollHook(p.onScroll)
	}
	if s, ok := vt.(interface{ SetBellHook(func()) }); ok {
		s.SetBellHook(p.onBell)
	}
	if s, ok := vt.(interface{ SetOSCHook(func(int, string)) }); ok {
		s.SetOSCHook(p.onOSC)
	}
	go p.readLoop()
	go p.waitLoop()
	return p, nil
}

// readLoop pumps pty output into the vt10x screen buffer and nudges the
// redraw channel (non-blocking: a pending redraw already covers new output).
func (p *Pane) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			// vt10x Write locks the state internally.
			_, _ = p.vt.Write(buf[:n])
			p.markOutput()
			select {
			case p.refresh <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *Pane) waitLoop() {
	_ = p.cmd.Wait()
	p.exited <- p
}

// onScroll captures lines scrolling off the top of the main screen. It is
// called by vt10x with the terminal lock held; rendering takes the same lock,
// so scrollback access stays consistent.
func (p *Pane) onScroll(lines [][]vt10x.Glyph) {
	p.scrollback = append(p.scrollback, lines...)
	if over := len(p.scrollback) - scrollbackCap; over > 0 {
		copy(p.scrollback, p.scrollback[over:])
		p.scrollback = p.scrollback[:scrollbackCap]
	}
	// Keep the view pinned to the same content while scrolled back.
	if p.scroll > 0 {
		p.scroll += len(lines)
		if p.scroll > len(p.scrollback) {
			p.scroll = len(p.scrollback)
		}
	}
}

// scrollBy moves the scrollback view by delta lines (positive = further
// back) and reports whether the pane is still scrolled.
func (p *Pane) scrollBy(delta int) bool {
	p.scroll += delta
	if p.scroll < 0 {
		p.scroll = 0
	}
	if p.scroll > len(p.scrollback) {
		p.scroll = len(p.scrollback)
	}
	return p.scroll > 0
}

// Resize updates both the pty and the virtual terminal size.
func (p *Pane) Resize(w, h int) {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if w == p.w && h == p.h {
		return
	}
	debugf("pane %d Resize %dx%d", p.id, w, h)
	p.w, p.h = w, h
	// vt10x Resize locks the state internally.
	p.vt.Resize(w, h)
	_ = pty.Setsize(p.ptmx, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
}

// Write forwards raw key bytes to the shell.
func (p *Pane) Write(b []byte) {
	_, _ = p.out.Write(b)
}

// Close kills the shell process and closes the pty.
func (p *Pane) Close() {
	if p.ptmx != nil {
		_ = p.ptmx.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// markOutput records output activity: updates the timestamp and raises the
// unread flag (the focus-clearing pass in Model.Update resets it for the
// focused pane continuously).
func (p *Pane) markOutput() {
	p.stateMu.Lock()
	p.lastOutput = time.Now()
	p.unread = true
	p.stateMu.Unlock()
}

// onBell is called by vt10x on every BEL received.
func (p *Pane) onBell() {
	p.stateMu.Lock()
	p.notified = true
	p.pushNoteLocked("bell")
	p.stateMu.Unlock()
}

// onOSC is called by vt10x on OSC 9/99/777 notification sequences; the
// payload is truncated and stored for display.
func (p *Pane) onOSC(_ int, payload string) {
	const maxNote = 80
	p.stateMu.Lock()
	p.notified = true
	p.oscNote = payload
	if len(p.oscNote) > maxNote {
		p.oscNote = p.oscNote[:maxNote] + "…"
	}
	p.pushNoteLocked(p.oscNote)
	p.stateMu.Unlock()
}

// noteEvent is one recorded notification in the pane's history.
type noteEvent struct {
	at   time.Time
	text string
}

// noteHistoryCap is the per-pane notification history ring size.
const noteHistoryCap = 20

// pushNoteLocked appends to the history ring; stateMu must be held. The
// history survives clearAttention — the red dot clearing does not erase it.
func (p *Pane) pushNoteLocked(text string) {
	p.notes = append(p.notes, noteEvent{at: time.Now(), text: text})
	if over := len(p.notes) - noteHistoryCap; over > 0 {
		copy(p.notes, p.notes[over:])
		p.notes = p.notes[:noteHistoryCap]
	}
}

// getNotes returns a copy of the pane's notification history (oldest first).
func (p *Pane) getNotes() []noteEvent {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	out := make([]noteEvent, len(p.notes))
	copy(out, p.notes)
	return out
}

// clearAttention resets unread/notify flags (called when the pane gains or
// holds the focus).
func (p *Pane) clearAttention() {
	p.stateMu.Lock()
	p.unread = false
	p.notified = false
	p.stateMu.Unlock()
}

// setMeta updates the meta fields collected by the periodic tick.
func (p *Pane) setMeta(proc, branch, agent string) {
	p.stateMu.Lock()
	p.proc = proc
	p.branch = branch
	p.agent = agent
	p.stateMu.Unlock()
}

// attention computes the pane's current attention level. Precedence:
// explicit notification (red) > unread / agent blocked (yellow) > active
// (recent output or agent working) > idle.
func (p *Pane) attention() attentionLevel {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.notified {
		return attnNotify
	}
	if p.unread {
		return attnUnread
	}
	if p.agent != "" {
		switch p.matchAgentScreen() {
		case attnUnread:
			return attnUnread
		case attnActive:
			return attnActive
		}
	}
	if time.Since(p.lastOutput) < activeWindow {
		return attnActive
	}
	return attnIdle
}

// matchAgentScreen scans the trailing screen lines for agent state
// patterns. Blocked (waiting-for-confirmation) rules are checked before
// working rules, so a stale working line never masks a fresh prompt.
// Returns attnIdle when nothing matches.
func (p *Pane) matchAgentScreen() attentionLevel {
	text := p.tailText(agentScreenLines)
	for _, r := range agentCfg.blocked {
		if r.re.MatchString(text) {
			return r.level
		}
	}
	for _, r := range agentCfg.working {
		if r.re.MatchString(text) {
			return r.level
		}
	}
	return attnIdle
}

// tailText returns up to n rows of the screen's *last non-blank* content
// as plain text (mirroring the mini-preview rule: trailing blank rows below
// the cursor are skipped).
func (p *Pane) tailText(n int) string {
	p.vt.Lock()
	defer p.vt.Unlock()
	cols, rows := p.vt.Size()
	last := -1
	for y := rows - 1; y >= 0; y-- {
		blank := true
		for x := 0; x < cols; x++ {
			if g := p.vt.Cell(x, y); g.Char != 0 && g.Char != ' ' {
				blank = false
				break
			}
		}
		if !blank {
			last = y
			break
		}
	}
	if last < 0 {
		return ""
	}
	start := last - n + 1
	if start < 0 {
		start = 0
	}
	var sb strings.Builder
	for y := start; y <= last; y++ {
		for x := 0; x < cols; x++ {
			if g := p.vt.Cell(x, y); g.Char != 0 {
				sb.WriteRune(g.Char)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// selectedText extracts the text between combined-buffer cells (x0,y0) and
// (x1,y1) — either direction, normalized here. Trailing spaces are trimmed
// per line and blank edge lines dropped, so the clipboard gets the content,
// not the padding. Wide-glyph dummy cells contribute nothing.
func (p *Pane) selectedText(x0, y0, x1, y1 int) string {
	if y0 > y1 || (y0 == y1 && x0 > x1) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	p.vt.Lock()
	defer p.vt.Unlock()
	cols, rows := p.vt.Size()
	total := len(p.scrollback) + rows
	if y0 >= total {
		return ""
	}
	if y1 >= total {
		y1 = total - 1
	}
	var sb strings.Builder
	for y := y0; y <= y1; y++ {
		var glyphs []vt10x.Glyph
		if y < len(p.scrollback) {
			glyphs = p.scrollback[y]
		} else {
			glyphs = make([]vt10x.Glyph, cols)
			for x := range glyphs {
				glyphs[x] = p.vt.Cell(x, y-len(p.scrollback))
			}
		}
		lo, hi := 0, cols-1
		if y == y0 {
			lo = x0
		}
		if y == y1 {
			hi = x1
		}
		if hi > cols-1 {
			hi = cols - 1
		}
		var lb strings.Builder
		for x := lo; x <= hi && x < len(glyphs); x++ {
			g := glyphs[x]
			if g.IsWideDummy() {
				continue
			}
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			lb.WriteRune(ch)
		}
		sb.WriteString(strings.TrimRight(lb.String(), " "))
		if y < y1 {
			sb.WriteByte('\n')
		}
	}
	return strings.Trim(sb.String(), "\n")
}
