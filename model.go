package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"pano/internal/vt10x"
)

// refreshMsg coalesces "some pane produced output" notifications.
type refreshMsg struct{}

// paneExitedMsg reports that a pane's shell process has exited.
type paneExitedMsg struct{ p *Pane }

// metaTickMsg fires the periodic meta collection (foreground process, git
// branch) and notice expiry.
type metaTickMsg struct{}

const metaTickPeriod = 2 * time.Second
const noticeTTL = 3 * time.Second
const cursorBlinkPeriod = 500 * time.Millisecond

func metaTick() tea.Cmd {
	return tea.Tick(metaTickPeriod, func(time.Time) tea.Msg { return metaTickMsg{} })
}

// cursorTickMsg drives the blinking terminal cursor.
type cursorTickMsg struct{}

func cursorTick() tea.Cmd {
	return tea.Tick(cursorBlinkPeriod, func(time.Time) tea.Msg { return cursorTickMsg{} })
}

// displayMode is the top-level view mode: grid (all panes tiled) or focus
// (one main view + sidebar minis).
type displayMode int

const (
	modeGrid displayMode = iota
	modeFocus
)

// dragThreshold is the movement in cells before a press on a border turns
// into a drag; below it the press is treated as a click (focus change).
const dragThreshold = 3

// doubleClickWindow is the maximum interval between two presses on a pane's
// title row that counts as a double-click (starts title editing).
const doubleClickWindow = 500 * time.Millisecond

// Model is the root bubbletea model.
type Model struct {
	panes []*Pane
	focus int

	leader      bool // prefix key (ctrl+g) pressed, waiting for command key
	editing     bool // title editor active on the focused pane
	scrolling   bool // scrollback mode active on the focused pane
	confirmQuit bool // quit confirmation pending
	closeSelect bool // close-target selection pending
	closeSel    int  // highlighted close target (defaults to the focus)

	// pendingRestore holds a saved session awaiting the startup [y/n]
	// prompt; while set, no default panes are created and input goes to the
	// prompt.
	pendingRestore *sessionFile

	titleInput textinput.Model

	// split-tree layout (grid mode)
	root      *node
	preset    preset
	mainRatio float64

	// focus-mode state
	mode        displayMode
	sidebarLeft bool // sidebar on the left (true) or right (false)
	sidebarW    int  // sidebar width (draggable; sidebarWidth by default)
	sbPending   bool // sidebar divider press recorded (not yet a drag)
	sbDragging  bool // sidebar divider drag in progress
	gridFocus   int  // focus to restore when leaving focus mode
	focusNav    bool // keyboard ownership: true = sidebar arrows navigate
	miniOffset  int  // first visible mini index when the sidebar overflows

	// recent-dirs picker state
	dirsOpen bool
	dirs     []dirEntry
	dirsSel  int

	// notification history drawer state
	notesOpen bool
	notes     []noteRow
	notesSel  int

	// border drag state: a press on a boundary records a pending drag; it
	// only becomes an active drag after moving dragThreshold cells.
	dragBounds []boundary
	dragging   bool
	pressX     int
	pressY     int
	pressPane  int
	lastX      int
	lastY      int

	// double-click detection for title editing
	lastClickPane int
	lastClickTime time.Time

	width, height int

	notice   string    // transient status-bar notice (F7 feedback)
	noticeAt time.Time // when it was set (hidden after noticeTTL)
	cursorOn bool      // blinking terminal cursor phase

	refreshCh chan struct{}
	exitCh    chan *Pane
	ctlCh     chan ctlMsg // external control commands (nil = disabled)
	nextID    int
	quitting  bool
}

func newModel() Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 64
	return Model{
		refreshCh:     make(chan struct{}, 1),
		exitCh:        make(chan *Pane, 16),
		titleInput:    ti,
		mainRatio:     defaultMain,
		sidebarLeft:   true,
		sidebarW:      sidebarWidth,
		cursorOn:      true,
		lastClickPane: -1,
		nextID:        1,
	}
}

func waitRefresh(ch chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		return refreshMsg{}
	}
}

func waitExit(ch chan *Pane) tea.Cmd {
	return func() tea.Msg {
		return paneExitedMsg{p: <-ch}
	}
}

// waitCtl blocks until an external control command arrives.
func waitCtl(ch chan ctlMsg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{waitRefresh(m.refreshCh), waitExit(m.exitCh), metaTick(), cursorTick()}
	if m.ctlCh != nil {
		cmds = append(cmds, waitCtl(m.ctlCh))
	}
	return tea.Batch(cmds...)
}

// contentArea is the screen area available for panes (the bottom row is the
// status bar).
func (m Model) contentArea() (w, h int) {
	h = m.height - 1
	if h < 1 {
		h = 1
	}
	return m.width, h
}

// leafOf returns the tree leaf holding pane p (nil if not found).
func (m *Model) leafOf(p *Pane) *node {
	path := leafPath(m.root, p)
	if path == nil {
		return nil
	}
	return path[len(path)-1]
}

// rects computes the current pane rects, ordered like m.panes. In focus
// mode it returns the sidebar mini rects with the focused pane's slot
// replaced by the main view rect.
func (m *Model) rects() []rect {
	w, h := m.contentArea()
	if m.mode == modeFocus {
		minis, main, _, _ := focusLayout(len(m.panes), w, h, m.focus, m.miniOffset, m.sidebarW, m.sidebarLeft)
		minis[m.focus] = main
		return minis
	}
	leaves := map[*Pane]rect{}
	layoutNode(m.root, rect{0, 0, w, h}, leaves, nil)
	out := make([]rect, len(m.panes))
	for i, p := range m.panes {
		out[i] = leaves[p]
	}
	return out
}

// clampMiniOffset applies the standard list-follow rules: the focused mini
// must stay inside the visible window, and the offset stays in range. Call
// after any focus/pane-count/size change.
func (m *Model) clampMiniOffset() {
	n := len(m.panes)
	_, h := m.contentArea()
	visible, overflowing := miniVisibleCount(n, h)
	if !overflowing {
		m.miniOffset = 0
		return
	}
	if m.focus < m.miniOffset {
		m.miniOffset = m.focus
	}
	if m.focus >= m.miniOffset+visible {
		m.miniOffset = m.focus - visible + 1
	}
	if m.miniOffset > n-visible {
		m.miniOffset = n - visible
	}
	if m.miniOffset < 0 {
		m.miniOffset = 0
	}
}

// layoutPanes resizes panes' pty + vt to match the current layout. Only the
// focused (main-view) pane is resized in focus mode; sidebar minis keep
// their size and are rendered truncated.
func (m *Model) layoutPanes() {
	if m.width == 0 || len(m.panes) == 0 {
		return
	}
	m.sidebarW = clampSidebarW(m.sidebarW, m.width)
	m.clampMiniOffset()
	rects := m.rects()
	for i, p := range m.panes {
		if m.mode == modeFocus && i != m.focus {
			continue
		}
		w, h := contentSize(rects[i])
		p.Resize(w, h)
	}
}

// addPane creates a pane (in dir, or pano's cwd when empty) and rebuilds the
// split tree as a balanced layout for the new pane count: every pane returns
// to an equal share (drag adjustments reset on add/remove — accepted
// semantics). Pane order is preserved, so the focus index keeps pointing at
// the same pane.
func (m *Model) addPane(dir string) {
	p, err := newPane(m.nextID, 80, 24, dir, m.refreshCh, m.exitCh)
	if err != nil {
		debugf("addPane: %v", err)
		return
	}
	m.nextID++
	m.panes = append(m.panes, p)
	m.root = buildTree(m.panes, m.preset, m.mainRatio)
	m.focus = len(m.panes) - 1
	m.layoutPanes()
}

// closePane removes pane i, kills its shell, and rebuilds the split tree as
// a balanced layout for the remaining panes. If it was the last pane the
// caller must quit.
func (m *Model) closePane(i int) {
	if i < 0 || i >= len(m.panes) {
		return
	}
	p := m.panes[i]
	m.panes = append(m.panes[:i], m.panes[i+1:]...)
	p.Close()
	m.root = buildTree(m.panes, m.preset, m.mainRatio)
	if m.focus >= len(m.panes) {
		m.focus = len(m.panes) - 1
	}
	if m.focus < 0 {
		m.focus = 0
	}
	if m.closeSel >= len(m.panes) {
		m.closeSel = len(m.panes) - 1
	}
	if m.closeSel < 0 {
		m.closeSel = 0
	}
	if m.gridFocus >= len(m.panes) {
		m.gridFocus = len(m.panes) - 1
	}
	if m.gridFocus < 0 {
		m.gridFocus = 0
	}
	m.scrolling = false
	m.layoutPanes()
}

// toggleFocusMode switches between grid mode and focus mode, remembering
// the grid focus.
func (m *Model) toggleFocusMode() {
	if m.mode == modeGrid {
		m.gridFocus = m.focus
		m.mode = modeFocus
		m.focusNav = false // start with the main terminal owning the keyboard
	} else {
		m.mode = modeGrid
		if m.gridFocus < len(m.panes) {
			m.focus = m.gridFocus
		}
	}
	m.scrolling = false
	m.layoutPanes()
}

// startTitleEdit opens the title editor for the focused pane.
func (m *Model) startTitleEdit() tea.Cmd {
	if len(m.panes) == 0 {
		return nil
	}
	m.editing = true
	m.titleInput.SetValue(m.panes[m.focus].title)
	cw, _ := contentSize(rect{0, 0, m.width / 2, m.height})
	m.titleInput.Width = cw - 2
	return m.titleInput.Focus()
}

// stepFocus moves focus: in focus mode vertical steps walk the sidebar
// order (index ±1); everything else picks the geometrically nearest pane in
// that direction.
func (m *Model) stepFocus(dx, dy int) {
	n := len(m.panes)
	if n < 2 {
		return
	}
	if m.mode == modeFocus && dx == 0 {
		next := m.focus + dy
		if next < 0 {
			next = 0
		}
		if next >= n {
			next = n - 1
		}
		if next != m.focus {
			m.focus = next
			m.layoutPanes()
		}
		m.focusNav = true // any selection move hands the arrows to the sidebar
		return
	}
	m.moveFocus(dx, dy)
}

// moveFocus moves focus to the nearest pane whose center lies in the given
// direction.
func (m *Model) moveFocus(dx, dy int) {
	if best := m.moveIndex(m.focus, dx, dy); best != m.focus {
		m.focus = best
		// The newly focused pane becomes the main view in focus mode.
		m.layoutPanes()
	}
}

// moveIndex returns the index of the pane best aligned with the given
// direction from pane `from`: candidates must lie in that direction, and we
// minimize the perpendicular offset first, then the parallel distance (so
// "right" prefers the pane straight to the right over a nearer diagonal
// one). Returns `from` when there is no candidate.
func (m *Model) moveIndex(from, dx, dy int) int {
	n := len(m.panes)
	if n < 2 || from < 0 || from >= n {
		return from
	}
	rects := m.rects()
	cur := rects[from]
	cx, cy := cur.x+cur.w/2, cur.y+cur.h/2
	best, bestDist := -1, math.MaxInt
	for i, r := range rects {
		if i == from {
			continue
		}
		rx, ry := r.x+r.w/2, r.y+r.h/2
		ddx, ddy := rx-cx, ry-cy
		if dx > 0 && ddx <= 0 || dx < 0 && ddx >= 0 ||
			dy > 0 && ddy <= 0 || dy < 0 && ddy >= 0 {
			continue
		}
		parallel, perpendicular := abs(ddx), abs(ddy)
		if dy != 0 {
			parallel, perpendicular = perpendicular, parallel
		}
		if dist := perpendicular*1000 + parallel; dist < bestDist {
			best, bestDist = i, dist
		}
	}
	if best < 0 {
		return from
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (m *Model) quit() tea.Cmd {
	m.quitting = true
	// Session resurrection: snapshot the layout before killing the shells
	// (their cwds are read from the process table); an empty session removes
	// the file instead.
	if path := sessionPath(); path != "" {
		if sf := m.sessionSnapshot(); sf != nil {
			_ = writeSession(path, sf)
		} else {
			_ = os.Remove(path)
		}
	}
	for _, p := range m.panes {
		p.Close()
	}
	return tea.Quit
}

// selfPIDs returns the pids of pano's own pane shells (excluded from the
// dirs picker's live-source to avoid self-references).
func (m *Model) selfPIDs() map[int]bool {
	out := map[int]bool{}
	for _, p := range m.panes {
		if p.cmd != nil && p.cmd.Process != nil {
			out[p.cmd.Process.Pid] = true
		}
	}
	return out
}

// noteRow is one entry in the merged notification history drawer: the pane
// index it came from, when it fired, and its text.
type noteRow struct {
	pane int
	at   time.Time
	text string
}

// collectNotes merges every pane's notification history, newest first.
func (m *Model) collectNotes() []noteRow {
	var out []noteRow
	for i, p := range m.panes {
		for _, n := range p.getNotes() {
			out = append(out, noteRow{pane: i, at: n.at, text: n.text})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].at.After(out[b].at) })
	return out
}

// runCommand executes a command key, shared by the buttons, leader mode and
// the direct Alt shortcuts. Reports whether the key is a bound command.
func (m *Model) runCommand(key string) (bool, tea.Cmd) {
	switch key {
	case "c", "n": // new terminal
		m.addPane("")
	case "x": // close: arm the close-target selection
		if len(m.panes) > 0 {
			m.closeSelect = true
			m.closeSel = m.focus
		}
	case "z": // legacy zoom key: now an alias for the focus-mode toggle
		m.toggleFocusMode()
	case "t":
		return true, m.startTitleEdit()
	case "q":
		// Never quit outright: arm the confirmation state first. The
		// second quit trigger (or "y") actually exits.
		m.confirmQuit = true
		return true, nil
	case "a": // jump to the next pane needing attention (F7)
		m.jumpAttention()
	case "o": // open the dirs picker (computed fresh every time)
		m.dirs = collectDirs(m.selfPIDs())
		m.dirsSel = 0
		m.dirsOpen = true
	case "N": // open the notification history drawer (F8)
		m.notes = m.collectNotes()
		if len(m.notes) == 0 {
			m.setNotice("no notifications")
			return true, nil
		}
		m.notesSel = 0
		m.notesOpen = true
	case "up", "k":
		m.stepFocus(0, -1)
	case "down", "j":
		m.stepFocus(0, 1)
	case "left", "h":
		m.stepFocus(-1, 0)
	case "right", "l":
		m.stepFocus(1, 0)
	case "H":
		if m.mode == modeFocus {
			m.sidebarW = clampSidebarW(m.sidebarW-4, m.width)
			m.layoutPanes()
		} else {
			nudgeSplit(m.root, m.panes[m.focus], splitH, -weightStep)
			m.layoutPanes()
		}
	case "L":
		if m.mode == modeFocus {
			m.sidebarW = clampSidebarW(m.sidebarW+4, m.width)
			m.layoutPanes()
		} else {
			nudgeSplit(m.root, m.panes[m.focus], splitH, weightStep)
			m.layoutPanes()
		}
	case "K":
		nudgeSplit(m.root, m.panes[m.focus], splitV, -weightStep)
		m.layoutPanes()
	case "J":
		nudgeSplit(m.root, m.panes[m.focus], splitV, weightStep)
		m.layoutPanes()
	case " ", "p": // cycle layout presets: rebuild the tree shape
		m.preset = (m.preset + 1) % presetCount
		m.root = buildTree(m.panes, m.preset, m.mainRatio)
		m.layoutPanes()
	case "tab", "m": // toggle grid <-> focus mode
		m.toggleFocusMode()
	case "b": // flip the focus-mode sidebar side
		m.sidebarLeft = !m.sidebarLeft
		m.layoutPanes()
	case "[", "pgup": // enter scrollback mode
		if len(m.panes) > 0 {
			m.scrolling = true
			if key == "pgup" {
				_, ch := contentSize(m.rects()[m.focus])
				m.panes[m.focus].scrollBy(ch - 1)
			}
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if n := int(key[0] - '1'); n < len(m.panes) && n != m.focus {
			m.focus = n
			m.layoutPanes() // focus change swaps the focus-mode main view
		}
	default:
		return false, nil
	}
	return true, nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	debugf("msg %T %+v", msg, msg)
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		debugf("window size %dx%d, panes=%d", m.width, m.height, len(m.panes))
		switch {
		case len(m.panes) > 0:
			m.layoutPanes()
		case m.pendingRestore != nil:
			// Waiting for the restore prompt: create no panes yet.
		default:
			m.addPane("")
			m.addPane("")
		}
		return m, nil

	case refreshMsg:
		return m, waitRefresh(m.refreshCh)

	case ctlMsg:
		// External control command: find the target pane by its stable id.
		for i, p := range m.panes {
			if p.id != msg.paneID {
				continue
			}
			switch msg.kind {
			case "notify":
				if msg.text == "" {
					p.onBell()
				} else {
					p.onOSC(777, msg.text)
				}
			case "focus":
				if i != m.focus {
					m.focus = i
					m.layoutPanes()
				}
			}
			break
		}
		if m.ctlCh != nil {
			return m, waitCtl(m.ctlCh)
		}
		return m, nil

	case cursorTickMsg:
		m.cursorOn = !m.cursorOn
		return m, cursorTick()

	case metaTickMsg:
		m.refreshMeta()
		if m.notice != "" && time.Since(m.noticeAt) > noticeTTL {
			m.notice = ""
		}
		return m, metaTick()

	case paneExitedMsg:
		if !m.quitting {
			for i, p := range m.panes {
				if p == msg.p {
					m.closePane(i)
					break
				}
			}
			if len(m.panes) == 0 {
				return m, m.quit()
			}
		}
		return m, waitExit(m.exitCh)

	case tea.KeyMsg:
		mm, cmd := m.handleKey(msg)
		m2 := mm.(Model)
		m2.clearFocusedAttention()
		return m2, cmd

	case tea.MouseMsg:
		mm, cmd := m.handleMouse(tea.MouseEvent(msg))
		m2 := mm.(Model)
		m2.clearFocusedAttention()
		return m2, cmd
	}
	m.clearFocusedAttention()
	return m, nil
}

// clearFocusedAttention resets the focused pane's unread/notify flags; it
// runs after every message so a pane's attention markers clear as soon as
// it is looked at.
func (m *Model) clearFocusedAttention() {
	if m.focus >= 0 && m.focus < len(m.panes) {
		m.panes[m.focus].clearAttention()
	}
}

// setNotice shows a transient status-bar notice.
func (m *Model) setNotice(s string) {
	m.notice = s
	m.noticeAt = time.Now()
}

// jumpAttention moves focus to the next pane needing attention (explicit
// notifications first, then unread/blocked), cycling. With nothing pending
// it only shows a notice.
func (m *Model) jumpAttention() {
	n := len(m.panes)
	if n == 0 {
		return
	}
	for _, level := range []attentionLevel{attnNotify, attnUnread} {
		for k := 1; k <= n; k++ {
			i := (m.focus + k) % n
			if m.panes[i].attention() >= level {
				m.focus = i
				m.layoutPanes()
				label := fmt.Sprintf("→ %d · %s", i+1, m.panes[i].title)
				m.panes[i].stateMu.Lock()
				if m.panes[i].oscNote != "" {
					label += " — " + m.panes[i].oscNote
				}
				m.panes[i].stateMu.Unlock()
				m.setNotice(label)
				return
			}
		}
	}
	m.setNotice("no attention")
}

// fKeyCommand maps function keys to command-table entries ("" = unbound,
// the key passes through to the shell).
func fKeyCommand(t tea.KeyType) string {
	switch t {
	case tea.KeyF2:
		return "n" // new terminal
	case tea.KeyF3:
		return "x" // close (select)
	case tea.KeyF4:
		return "m" // grid <-> focus mode
	case tea.KeyF5:
		return "p" // layout preset cycle
	case tea.KeyF6:
		return "o" // recent dirs
	case tea.KeyF7:
		return "a" // jump to next attention
	case tea.KeyF8:
		return "N" // notification history
	case tea.KeyF9:
		return "q" // quit (confirm)
	}
	return ""
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Session restore prompt: "y"/"Y" restores the saved session; any other
	// key declines (the saved file is discarded) and starts the default two
	// panes. The key is swallowed either way.
	if m.pendingRestore != nil {
		sf := m.pendingRestore
		m.pendingRestore = nil
		if s := msg.String(); s == "y" || s == "Y" {
			m.restoreSession(sf)
		} else {
			if path := sessionPath(); path != "" {
				_ = os.Remove(path)
			}
			m.addPane("")
			m.addPane("")
		}
		return m, nil
	}

	// Quit confirmation: "y"/"Y" exits; any other key cancels and is
	// swallowed (nothing reaches the shell, no other action fires).
	if m.confirmQuit {
		m.confirmQuit = false
		if s := msg.String(); s == "y" || s == "Y" {
			return m, m.quit()
		}
		return m, nil
	}

	// Close-target selection: pick which pane to close.
	if m.closeSelect {
		s := msg.String()
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			s = string(msg.Runes[0]) // first rune only; the rest is dropped
		}
		switch s {
		case "up", "k":
			m.closeSel = m.moveIndex(m.closeSel, 0, -1)
			return m, nil
		case "down", "j":
			m.closeSel = m.moveIndex(m.closeSel, 0, 1)
			return m, nil
		case "left", "h":
			m.closeSel = m.moveIndex(m.closeSel, -1, 0)
			return m, nil
		case "right", "l":
			m.closeSel = m.moveIndex(m.closeSel, 1, 0)
			return m, nil
		}
		m.closeSelect = false
		closeIdx := -1
		switch {
		case msg.Type == tea.KeyEnter:
			closeIdx = m.closeSel
		case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
			if n := int(s[0] - '1'); n < len(m.panes) {
				closeIdx = n
			}
		}
		if closeIdx >= 0 {
			m.closePane(closeIdx)
			if len(m.panes) == 0 {
				return m, m.quit()
			}
		}
		return m, nil // anything else: cancel, swallowed
	}

	// Dirs picker.
	if m.dirsOpen {
		limit := len(m.dirs)
		if limit > dirsPickerRows {
			limit = dirsPickerRows
		}
		switch msg.String() {
		case "up", "k":
			if m.dirsSel > 0 {
				m.dirsSel--
			}
		case "down", "j":
			if m.dirsSel < limit-1 {
				m.dirsSel++
			}
		case "enter":
			if limit > 0 {
				dir := m.dirs[m.dirsSel].path
				m.dirsOpen = false
				m.addPane(dir)
				return m, nil
			}
			m.dirsOpen = false // empty list: Enter just closes
		default:
			m.dirsOpen = false // cancel, swallowed
		}
		return m, nil
	}

	// Notification history drawer.
	if m.notesOpen {
		limit := len(m.notes)
		if limit > notesPickerRows {
			limit = notesPickerRows
		}
		switch msg.String() {
		case "up", "k":
			if m.notesSel > 0 {
				m.notesSel--
			}
		case "down", "j":
			if m.notesSel < limit-1 {
				m.notesSel++
			}
		case "enter":
			row := m.notes[m.notesSel]
			m.notesOpen = false
			if row.pane >= 0 && row.pane < len(m.panes) {
				m.focus = row.pane
			}
		default:
			m.notesOpen = false // cancel, swallowed
		}
		return m, nil
	}

	// Scrollback mode: navigation keys scroll, anything else exits.
	if m.scrolling {
		p := m.panes[m.focus]
		_, ch := contentSize(m.rects()[m.focus])
		switch msg.String() {
		case "up", "k":
			p.scrollBy(1)
		case "down", "j":
			p.scrollBy(-1)
		case "pgup":
			p.scrollBy(ch - 1)
		case "pgdown":
			p.scrollBy(-(ch - 1))
		default:
			m.scrolling = false
			p.scroll = 0
			return m, nil
		}
		if p.scroll == 0 {
			m.scrolling = false
		}
		return m, nil
	}

	// Title editing mode: keys go to the textinput, not the shell.
	if m.editing {
		switch msg.Type {
		case tea.KeyEnter:
			if t := strings.TrimSpace(m.titleInput.Value()); t != "" {
				m.panes[m.focus].title = t
			}
			m.editing = false
			m.titleInput.Blur()
			return m, nil
		case tea.KeyEsc:
			m.editing = false
			m.titleInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.titleInput, cmd = m.titleInput.Update(msg)
		return m, cmd
	}

	// Focus-mode sidebar keyboard ownership: while the sidebar owns the
	// keyboard, arrows navigate the selection (the main view follows) and
	// any other key hands the keyboard to the main view and is processed
	// normally (i.e. passed through to its shell).
	if m.mode == modeFocus && m.focusNav {
		switch msg.String() {
		case "up", "k":
			m.stepFocus(0, -1)
			return m, nil
		case "down", "j":
			m.stepFocus(0, 1)
			return m, nil
		case "left", "h", "right", "l":
			return m, nil // no-op in sidebar nav
		default:
			m.focusNav = false
			// fall through to normal processing
		}
	}

	// Leader mode: the next key is a command.
	if m.leader {
		m.leader = false

		// bubbletea may coalesce fast-typed runes into one message (e.g.
		// "tmyterm" or "LL"): the first rune is the command. Remaining runes
		// are NEVER forwarded to a shell — they only feed the title editor
		// when the command just opened it; otherwise they are dropped.
		key := msg.String()
		var rest []rune
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			key = string(msg.Runes[0])
			rest = msg.Runes[1:]
		}

		_, cmd := m.runCommand(key)

		if len(rest) > 0 && m.editing {
			var c tea.Cmd
			m.titleInput, c = m.titleInput.Update(tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: rest}))
			cmd = tea.Batch(cmd, c)
		}
		return m, cmd
	}

	// Direct Alt shortcuts: same commands, no leader needed.
	if tea.Key(msg).Alt {
		return m.handleAlt(tea.Key(msg))
	}

	// Function keys: bound F keys run commands, unbound ones pass through.
	if name := fKeyCommand(msg.Type); name != "" {
		_, cmd := (&m).runCommand(name)
		return m, cmd
	}

	if msg.Type == tea.KeyCtrlG {
		m.leader = true
		return m, nil
	}

	// Pass-through: everything else goes to the focused shell.
	if len(m.panes) == 0 {
		return m, nil
	}
	p := m.panes[m.focus]
	p.vt.Lock()
	appCursor := p.vt.Mode()&vt10x.ModeAppCursor != 0
	p.vt.Unlock()
	if b := keyBytes(msg, appCursor); b != nil {
		p.Write(b)
	}
	return m, nil
}

// handleAlt routes Alt+key to the shared command table. Unbound Alt keys
// pass through to the shell as ESC-prefixed sequences (readline semantics).
func (m Model) handleAlt(k tea.Key) (tea.Model, tea.Cmd) {
	var names []string
	if k.Type == tea.KeyRunes {
		// Alt runes arrive one per message, but handle a batch just in case.
		for _, r := range k.Runes {
			names = append(names, string(r))
		}
	} else {
		names = []string{strings.TrimPrefix(k.String(), "alt+")}
	}

	handled := false
	var cmds []tea.Cmd
	for _, name := range names {
		if ok, cmd := (&m).runCommand(name); ok {
			handled = true
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	if handled {
		return m, tea.Batch(cmds...)
	}

	// Unbound: forward as ESC prefix + key bytes.
	if len(m.panes) > 0 {
		p := m.panes[m.focus]
		p.vt.Lock()
		appCursor := p.vt.Mode()&vt10x.ModeAppCursor != 0
		p.vt.Unlock()
		if b := keyBytes(tea.KeyMsg(k), appCursor); b != nil {
			p.Write(b)
		}
	}
	return m, nil
}

// paneAt returns the index of the pane whose rect (border included) contains
// (x, y), or -1.
func (m *Model) paneAt(x, y int) int {
	for i, r := range m.rects() {
		if x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h {
			return i
		}
	}
	return -1
}

func (m Model) handleMouse(ev tea.MouseEvent) (tea.Model, tea.Cmd) {
	// Restore prompt pending: the mouse does nothing — answering is
	// keyboard-only so an accidental click can never discard the session.
	if m.pendingRestore != nil {
		return m, nil
	}

	// Title editing: a click outside the editor (the focused pane's title
	// row) commits the edit like Enter, then the click proceeds with its
	// normal semantics (focus change, promote, button action…).
	if m.editing && ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft && len(m.panes) > 0 {
		r := m.rects()[m.focus]
		inEditor := ev.Y == r.y && ev.X >= r.x && ev.X < r.x+r.w
		if !inEditor {
			if t := strings.TrimSpace(m.titleInput.Value()); t != "" {
				m.panes[m.focus].title = t
			}
			m.editing = false
			m.titleInput.Blur()
			// fall through: the click now behaves normally
		}
	}

	// Quit confirmation pending: a click on the [quit] button confirms;
	// any other click (or wheel) just cancels. Releases and motion events
	// are ignored — in particular the release of the arming click must not
	// cancel the confirmation it just raised.
	if m.confirmQuit {
		if ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft {
			m.confirmQuit = false
			if ev.Y == m.height-1 {
				_, zones := m.statusBarLayout(m.width)
				for _, z := range zones {
					if z.id == "quit" && ev.X >= z.x0 && ev.X < z.x1 {
						return m, m.quit()
					}
				}
			}
			return m, nil
		}
		if ev.IsWheel() {
			m.confirmQuit = false
		}
		return m, nil
	}

	// Close-target selection pending: a click on a pane closes it; any
	// other press cancels. Releases/motion are ignored.
	if m.closeSelect {
		if ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft {
			m.closeSelect = false
			if ev.Y == m.height-1 {
				return m, nil // status bar click: cancel only
			}
			if i := m.paneAt(ev.X, ev.Y); i >= 0 {
				m.closePane(i)
				if len(m.panes) == 0 {
					return m, m.quit()
				}
			}
			return m, nil
		}
		if ev.IsWheel() {
			m.closeSelect = false
		}
		return m, nil
	}

	// Recent-dirs picker: click a row to open a pane there; click anywhere
	// else cancels. The wheel moves the selection.
	if m.dirsOpen {
		limit := len(m.dirs)
		if limit > dirsPickerRows {
			limit = dirsPickerRows
		}
		if ev.IsWheel() {
			switch ev.Button {
			case tea.MouseButtonWheelUp:
				if m.dirsSel > 0 {
					m.dirsSel--
				}
			case tea.MouseButtonWheelDown:
				if m.dirsSel < limit-1 {
					m.dirsSel++
				}
			}
			return m, nil
		}
		if ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft {
			m.dirsOpen = false
			startY, lines := m.dirsOverlayGeometry()
			row := ev.Y - startY
			if row >= 1 && row < len(lines) && row-1 < limit {
				m.addPane(m.dirs[row-1].path)
			}
			return m, nil
		}
		return m, nil
	}

	// Notification history drawer: click a row to jump to its pane; click
	// anywhere else cancels. The wheel moves the selection.
	if m.notesOpen {
		limit := len(m.notes)
		if limit > notesPickerRows {
			limit = notesPickerRows
		}
		if ev.IsWheel() {
			switch ev.Button {
			case tea.MouseButtonWheelUp:
				if m.notesSel > 0 {
					m.notesSel--
				}
			case tea.MouseButtonWheelDown:
				if m.notesSel < limit-1 {
					m.notesSel++
				}
			}
			return m, nil
		}
		if ev.Action == tea.MouseActionPress && ev.Button == tea.MouseButtonLeft {
			m.notesOpen = false
			startY, lines := m.notesOverlayGeometry()
			row := ev.Y - startY
			if row >= 1 && row < len(lines) && row-1 < limit {
				if i := m.notes[row-1].pane; i >= 0 && i < len(m.panes) {
					m.focus = i
				}
			}
			return m, nil
		}
		return m, nil
	}

	// Mouse wheel: over the focus-mode sidebar it moves the selection;
	// otherwise it scrolls the pane under the cursor (also focusing it).
	if ev.IsWheel() {
		i := m.paneAt(ev.X, ev.Y)
		if m.mode == modeFocus {
			w, _ := m.contentArea()
			sideX0, sideX1 := 0, m.sidebarW
			if !m.sidebarLeft {
				sideX0, sideX1 = w-m.sidebarW, w
			}
			if i < 0 && ev.X >= sideX0 && ev.X < sideX1 {
				i = -2 // sidebar column but not a visible mini: still navigates
			}
			if i == -2 || (i >= 0 && i != m.focus) {
				switch ev.Button {
				case tea.MouseButtonWheelUp:
					m.stepFocus(0, -1)
				case tea.MouseButtonWheelDown:
					m.stepFocus(0, 1)
				}
				return m, nil
			}
		}
		if i < 0 {
			return m, nil
		}
		m.focus = i
		p := m.panes[i]
		switch ev.Button {
		case tea.MouseButtonWheelUp:
			m.scrolling = true
			p.scrollBy(3)
		case tea.MouseButtonWheelDown:
			if !p.scrollBy(-3) {
				m.scrolling = false
			}
		}
		return m, nil
	}

	switch ev.Action {
	case tea.MouseActionPress:
		if ev.Button != tea.MouseButtonLeft {
			return m, nil
		}
		// Status bar buttons: hit zones come from the same layout function
		// used for rendering, and dispatch to the same command table as the
		// keyboard shortcuts.
		if ev.Y == m.height-1 {
			_, zones := m.statusBarLayout(m.width)
			cmdKeys := map[string]string{
				"new": "n", "close": "x", "mode": "m",
				"layout": "p", "dirs": "o", "notes": "N", "quit": "q",
			}
			for _, z := range zones {
				if ev.X >= z.x0 && ev.X < z.x1 {
					_, cmd := (&m).runCommand(cmdKeys[z.id])
					return m, cmd
				}
			}
			return m, nil
		}
		m.pressX, m.pressY = ev.X, ev.Y
		m.pressPane = m.paneAt(ev.X, ev.Y)
		// Border drags only exist in the grid; in focus mode the sidebar
		// minis never drag — a press there is always a click. The sidebar
		// divider itself is draggable (pending until the threshold).
		m.dragBounds = nil
		if m.mode == modeFocus {
			divX := m.sidebarW
			if !m.sidebarLeft {
				divX = m.width - m.sidebarW
			}
			if abs(ev.X-divX) <= 1 {
				m.sbPending = true
				m.sbDragging = false
				return m, nil // focus/click decision deferred to release
			}
		}
		if m.mode == modeGrid {
			m.dragBounds = findBoundaries(m.rects(), ev.X, ev.Y)
			if len(m.dragBounds) > 1 && m.pressPane >= 0 {
				// At a junction several boundaries coincide; only drag the
				// ones incident to the pane under the press, so dragging
				// pane X's corner resizes exactly pane X and its direct
				// neighbors.
				var incident []boundary
				for _, b := range m.dragBounds {
					if b.a == m.pressPane || b.b == m.pressPane {
						incident = append(incident, b)
					}
				}
				if len(incident) > 0 {
					m.dragBounds = incident
				}
			}
		}
		m.dragging = false
		if len(m.dragBounds) == 0 && m.pressPane >= 0 {
			// Plain click away from any boundary: focus immediately, and
			// detect a double-click on the title row to start renaming.
			// Keyboard ownership in focus mode: clicking a mini hands the
			// arrows to the sidebar; clicking the main view hands them to
			// the terminal.
			if m.mode == modeFocus {
				if m.pressPane == m.focus {
					m.focusNav = false
				} else {
					m.focus = m.pressPane
					m.focusNav = true
				}
			} else {
				m.focus = m.pressPane
				// Click-promote: the clicked pane takes promoteRatio in each
				// split along its path (grid-mode mouse clicks only).
				promotePane(m.root, m.panes[m.focus])
			}
			m.layoutPanes()
			r := m.rects()[m.pressPane]
			if ev.Y == r.y {
				now := time.Now()
				if m.lastClickPane == m.pressPane && now.Sub(m.lastClickTime) < doubleClickWindow {
					m.lastClickPane = -1
					return m, m.startTitleEdit()
				}
				m.lastClickPane, m.lastClickTime = m.pressPane, now
			}
		}
		return m, nil

	case tea.MouseActionMotion:
		// Sidebar divider drag (focus mode): threshold first, then live.
		if m.sbPending {
			if !m.sbDragging {
				if abs(ev.X-m.pressX) < dragThreshold {
					return m, nil
				}
				m.sbDragging = true
				m.lastX = m.pressX
			}
			if delta := ev.X - m.lastX; delta != 0 {
				if m.sidebarLeft {
					m.sidebarW = clampSidebarW(m.sidebarW+delta, m.width)
				} else {
					m.sidebarW = clampSidebarW(m.sidebarW-delta, m.width)
				}
				m.lastX = ev.X
				m.layoutPanes()
			}
			return m, nil
		}
		if len(m.dragBounds) == 0 {
			return m, nil
		}
		if !m.dragging {
			if abs(ev.X-m.pressX) < dragThreshold && abs(ev.Y-m.pressY) < dragThreshold {
				return m, nil // still a click, not a drag yet
			}
			m.dragging = true
			m.lastX, m.lastY = m.pressX, m.pressY
		}
		dx, dy := ev.X-m.lastX, ev.Y-m.lastY
		if dx != 0 || dy != 0 {
			// A corner hit can match several boundaries that resolve to the
			// same split node — adjust each node only once.
			seen := map[*node]bool{}
			for _, b := range m.dragBounds {
				m.dragBoundaryBy(b, dx, dy, seen)
			}
			m.lastX, m.lastY = ev.X, ev.Y
			m.layoutPanes()
		}
		return m, nil

	case tea.MouseActionRelease:
		// Sidebar divider: a press without a drag is just a pane click.
		if m.sbPending {
			wasDragging := m.sbDragging
			m.sbPending, m.sbDragging = false, false
			if !wasDragging && m.pressPane >= 0 {
				if m.mode == modeFocus {
					if m.pressPane == m.focus {
						m.focusNav = false
					} else {
						m.focus = m.pressPane
						m.focusNav = true
					}
				} else {
					m.focus = m.pressPane
				}
				m.layoutPanes()
			}
			return m, nil
		}
		if len(m.dragBounds) > 0 && !m.dragging && m.pressPane >= 0 {
			// Press on a boundary without movement: treat as a click (and
			// promote the clicked pane, like a plain grid click).
			m.focus = m.pressPane
			promotePane(m.root, m.panes[m.focus])
			m.layoutPanes()
		}
		m.dragging = false
		m.dragBounds = nil
		return m, nil
	}
	return m, nil
}

// dragBoundaryBy adjusts the ratio of the split node that separates the two
// panes of b by the drag delta (dx for vertical borders, dy for horizontal).
// Only the two subtrees under that node change size. Nodes in `seen` are
// skipped (a corner can resolve multiple boundaries to the same split).
func (m *Model) dragBoundaryBy(b boundary, dx, dy int, seen map[*node]bool) {
	if m.root == nil || len(m.panes) < 2 {
		return
	}
	dir := splitH
	d := dx
	if !b.vertical {
		dir = splitV
		d = dy
	}
	nd := findSplitNode(m.root, m.panes[b.a], m.panes[b.b], dir)
	if nd == nil || seen[nd] {
		return
	}
	seen[nd] = true
	w, h := m.contentArea()
	nodes := map[*node]rect{}
	layoutNode(m.root, rect{0, 0, w, h}, nil, nodes)
	r := nodes[nd]
	size := r.w
	if dir == splitV {
		size = r.h
	}
	if size < 2 {
		return
	}
	delta := float64(d) / float64(size)
	if !subtreeHas(nd.a, m.panes[b.a]) {
		delta = -delta
	}
	nd.ratio = clampf(nd.ratio+delta, minRatio, maxRatio)
}

func (m Model) View() string {
	if m.width == 0 {
		return ""
	}
	// With no panes (only possible while the restore prompt is pending) the
	// body carries a big centered prompt — a blank screen with just a status
	// bar line reads as "broken".
	if len(m.panes) == 0 {
		w, h := m.contentArea()
		lines := make([]string, h)
		if m.pendingRestore != nil {
			sf := m.pendingRestore
			prompt := quitTextStyle.Render(fmt.Sprintf(" restore previous session (%d terminals)? ", len(sf.Panes)))
			hint := statusKeyStyle.Render("[y] restore · [any other key] fresh start")
			saved := statusKeyStyle.Render("saved "+sf.SavedAt.Format("2006-01-02 15:04:05"))
			center := []string{"", prompt, hint, saved}
			top := (h - len(center)) / 2
			if top < 0 {
				top = 0
			}
			for i, cl := range center {
				cw := ansi.StringWidth(cl)
				pad := (w - cw) / 2
				if pad < 0 {
					pad = 0
				}
				if top+i < h {
					lines[top+i] = strings.Repeat(" ", pad) + cl
				}
			}
		}
		return lipgloss.JoinVertical(lipgloss.Left, strings.Join(lines, "\n"), m.renderStatusBar())
	}
	var body string
	{
		w, h := m.contentArea()
		var paneRects []rect
		var rendered []string
		if m.mode == modeFocus {
			// Sidebar minis (real content, bottom aligned) + the focused
			// pane in the main view + scroll indicators when overflowing.
			minis, main, moreUp, moreDown := focusLayout(len(m.panes), w, h, m.focus, m.miniOffset, m.sidebarW, m.sidebarLeft)
			for i, r := range minis {
				if r.w <= 0 || r.h <= 0 {
					continue
				}
				paneRects = append(paneRects, r)
				rendered = append(rendered, m.renderMini(i, r))
			}
			paneRects = append(paneRects, main)
			rendered = append(rendered, m.renderPane(m.focus, main))
			if moreUp+moreDown > 0 {
				sideX := 0
				if !m.sidebarLeft {
					sideX = w - m.sidebarW
				}
				paneRects = append(paneRects,
					rect{sideX, 0, m.sidebarW, 1},
					rect{sideX, h - 1, m.sidebarW, 1})
				rendered = append(rendered,
					renderMiniIndicator(true, moreUp, m.sidebarW),
					renderMiniIndicator(false, moreDown, m.sidebarW))
			}
		} else {
			for i, r := range m.rects() {
				paneRects = append(paneRects, r)
				rendered = append(rendered, m.renderPane(i, r))
			}
		}
		body = composeBody(rendered, paneRects, w, h)
	}
	if m.dirsOpen {
		body = m.overlayDirs(body)
	}
	if m.notesOpen {
		body = m.overlayNotes(body)
	}
	return lipgloss.JoinVertical(lipgloss.Left, body, m.renderStatusBar())
}

// composeBody places each rendered pane at its rect position, producing
// exactly h lines of w cells. Works for any tiling (grid rows, main-left's
// full-height main pane, focus mode's sidebar+main) — panes are positioned
// independently instead of being grouped into horizontal rows.
func composeBody(rendered []string, rects []rect, w, h int) string {
	type seg struct {
		x    int
		text string
	}
	rows := make([][]seg, h)
	for i, r := range rects {
		for dy, line := range strings.Split(rendered[i], "\n") {
			if y := r.y + dy; y >= 0 && y < h {
				rows[y] = append(rows[y], seg{r.x, line})
			}
		}
	}
	var sb strings.Builder
	for y := 0; y < h; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		segs := rows[y]
		sort.Slice(segs, func(a, b int) bool { return segs[a].x < segs[b].x })
		x := 0
		for _, sg := range segs {
			if sg.x > x {
				sb.WriteString(strings.Repeat(" ", sg.x-x))
			}
			sb.WriteString(sg.text)
			x = sg.x + ansi.StringWidth(sg.text)
		}
		if x < w {
			sb.WriteString(strings.Repeat(" ", w-x))
		}
	}
	return sb.String()
}
