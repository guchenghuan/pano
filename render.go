package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"pano/internal/vt10x"
)

// lipglossColor maps a vt10x color to a lipgloss color.
//
// vt10x stores colors as: ANSI/palette index [0,256), truecolor as a packed
// 0xRRGGBB value, and DefaultFG/DefaultBG as 1<<24 and 1<<24+1. Defaults map
// to nil so the outer terminal's own default colors show through.
func lipglossColor(c vt10x.Color) lipgloss.TerminalColor {
	switch c {
	case vt10x.DefaultFG, vt10x.DefaultBG:
		return nil
	}
	if c < 256 {
		// ANSI 0-15 and the xterm 256 palette both map to the same numeric
		// ANSI color sequence.
		return lipgloss.Color(strconv.Itoa(int(c)))
	}
	// Truecolor, packed as 0xRRGGBB (always < 1<<24).
	return lipgloss.Color(fmt.Sprintf("#%06x", uint32(c)))
}

// styleKey identifies the visual style of a run of adjacent cells.
type styleKey struct {
	fg, bg                  vt10x.Color
	cur                     bool // our hardware cursor cell: white block
	inv                     bool // app-drawn reverse cell with default colors (e.g. Ink cursor)
	sel                     bool // inside the mouse text selection: reverse
	bold, italic, underline bool
}

var plainKey = styleKey{fg: vt10x.DefaultFG, bg: vt10x.DefaultBG}

// viewSel is a normalized mouse text selection in view-line coordinates
// (0,0 = top-left body cell); active=false means no selection.
type viewSel struct {
	active         bool
	ax, ay, bx, by int
}

func (s viewSel) contains(x, y int) bool {
	if !s.active || y < s.ay || y > s.by {
		return false
	}
	if s.ay == s.by {
		return x >= s.ax && x <= s.bx
	}
	if y == s.ay {
		return x >= s.ax
	}
	if y == s.by {
		return x <= s.bx
	}
	return true
}

func (k styleKey) style() lipgloss.Style {
	if k.cur {
		// Our hardware cursor: white block, dark glyph.
		return lipgloss.NewStyle().Background(cursorColor).Foreground(frameBlur)
	}
	if k.inv {
		// Reverse video of default colors (FG=DefaultBG, BG=DefaultFG) —
		// how Ink-based apps draw their cursor. Emit a bare SGR reverse with
		// no explicit colors: k.fg/k.bg hold palette 0 here, and applying
		// them would swap black-on-black into an invisible black block.
		return lipgloss.NewStyle().Reverse(true)
	}
	st := lipgloss.NewStyle()
	if c := lipglossColor(k.fg); c != nil {
		st = st.Foreground(c)
	}
	if c := lipglossColor(k.bg); c != nil {
		st = st.Background(c)
	}
	// SGR rendition pass-through: child apps' bold headings, italics and
	// underlined links survive the trip to the outer terminal.
	if k.bold {
		st = st.Bold(true)
	}
	if k.italic {
		st = st.Italic(true)
	}
	if k.underline {
		st = st.Underline(true)
	}
	if k.sel {
		// Mouse text selection: reverse video, same as native terminal
		// selection looks.
		st = st.Reverse(true)
	}
	return st
}

// renderGlyphs renders h rows of glyph lines into exactly h strings of w
// display cells each, merging adjacent cells that share the same colors into
// a single styled run. cursorX/cursorY mark the cursor cell (-1 = none);
// sel marks the mouse text selection in view-line coordinates.
func renderGlyphs(lines [][]vt10x.Glyph, w, h, cursorX, cursorY int, sel viewSel) string {
	var sb strings.Builder
	for y := 0; y < h; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}

		var line, run strings.Builder
		runKey := plainKey
		runStyled := false
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runStyled {
				line.WriteString(runKey.style().Render(run.String()))
			} else {
				line.WriteString(run.String())
			}
			run.Reset()
		}

		var glyphs []vt10x.Glyph
		if y < len(lines) {
			glyphs = lines[y]
		}
		for x := 0; x < w; x++ {
			ch := ' '
			key := plainKey
			if x < len(glyphs) {
				g := glyphs[x]
				if g.IsWideDummy() {
					// Trailing cell of a double-width glyph: emits
					// nothing, the glyph already spans both cells.
					continue
				}
				if g.Char != 0 {
					ch = g.Char
				}
				b, i, u := g.Style()
				key = styleKey{fg: g.FG, bg: g.BG, bold: b, italic: i, underline: u}
				if g.FG == vt10x.DefaultBG && g.BG == vt10x.DefaultFG {
					// Reversed default cell: an application-drawn cursor or
					// highlight block. It must not render as plain space.
					key = styleKey{inv: true}
				}
			}
			if cursorX == x && cursorY == y {
				key.cur = true
			}
			if sel.contains(x, y) {
				key.sel = true
			}
			styled := key != plainKey
			if styled != runStyled || (styled && key != runKey) {
				flush()
				runKey = key
				runStyled = styled
			}
			run.WriteRune(ch)
		}
		flush()

		// Normalize the line to exactly w display cells.
		s := line.String()
		switch dw := ansi.StringWidth(s); {
		case dw < w:
			s += strings.Repeat(" ", w-dw)
		case dw > w:
			s = ansi.Truncate(s, w, "")
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// vtRow copies one screen row out of the virtual terminal.
func vtRow(vt vt10x.Terminal, y, cols int) []vt10x.Glyph {
	row := make([]vt10x.Glyph, cols)
	for x := 0; x < cols; x++ {
		row[x] = vt.Cell(x, y)
	}
	return row
}

// renderVT dumps the live vt10x screen buffer, h lines of w cells.
func renderVT(vt vt10x.Terminal, w, h int, showCursor bool, sel viewSel) string {
	vt.Lock()
	defer vt.Unlock()

	cols, rows := vt.Size()
	lines := make([][]vt10x.Glyph, 0, h)
	for y := 0; y < h && y < rows; y++ {
		lines = append(lines, vtRow(vt, y, cols))
	}
	cx, cy := -1, -1
	if showCursor && vt.CursorVisible() {
		cur := vt.Cursor()
		cx, cy = cur.X, cur.Y
	}
	return renderGlyphs(lines, w, h, cx, cy, sel)
}

// overlayText centers a dim placeholder text in the middle line of s.
func overlayText(s string, w, h int, text string) string {
	lines := strings.Split(s, "\n")
	mid := h / 2
	if mid >= len(lines) {
		return s
	}
	pad := (w - len(text)) / 2
	if pad < 0 {
		pad = 0
	}
	lines[mid] = strings.Repeat(" ", pad) + blurTitleStyle.Render(text)
	// Normalize width.
	dw := ansi.StringWidth(lines[mid])
	if dw < w {
		lines[mid] += strings.Repeat(" ", w-dw)
	}
	return strings.Join(lines, "\n")
}

// decoRunes are characters that carry no information in a tail preview: a
// line made only of these (box frames, separators) is skipped in favor of
// real content lines above it.
const decoRunes = " ─│┌┐└┘├┤┬┴┼═║╔╗╚╝╭╮╰╯"

// meaningfulLine reports whether s contains any non-decoration character.
func meaningfulLine(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(decoRunes, r) {
			return true
		}
	}
	return false
}

// compressPreviewLine squeezes a tail line for the mini preview: leading
// indentation is dropped and interior whitespace runs collapse to one
// space, so substantially more real content fits the narrow width while the
// text stays fully readable.
func compressPreviewLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// renderMiniBody renders the focus-mode mini body as a live tail preview:
// the pane's trailing meaningful text lines (whitespace-compressed),
// bottom-aligned, so a glance answers "what is this task / is it done"
// (scrolling build output while running, `ok`/FAIL or the shell/agent
// prompt when finished). Pure separator/border lines are skipped so the
// scarce rows carry content; a blank screen shows a dim placeholder.
func renderMiniBody(p *Pane, w, h int) string {
	var raw []string
	if tail := p.tailText(h + 6); tail != "" {
		raw = strings.Split(tail, "\n")
	}
	picked := make([]string, 0, h)
	in := map[int]bool{}
	for i := len(raw) - 1; i >= 0 && len(picked) < h; i-- {
		if meaningfulLine(raw[i]) {
			picked = append([]string{raw[i]}, picked...)
			in[i] = true
		}
	}
	// Not enough content lines: fill from the skipped decoration rows.
	for i := len(raw) - 1; i >= 0 && len(picked) < h; i-- {
		if !in[i] {
			picked = append([]string{raw[i]}, picked...)
		}
	}

	lines := make([]string, h)
	for i := 0; i < h; i++ {
		line := ""
		if j := i - (h - len(picked)); j >= 0 { // bottom-aligned
			line = compressPreviewLine(picked[j])
		}
		switch dw := ansi.StringWidth(line); {
		case dw < w:
			line += strings.Repeat(" ", w-dw)
		case dw > w:
			line = ansi.Truncate(line, w, "")
		}
		lines[i] = line
	}
	body := strings.Join(lines, "\n")
	if len(picked) == 0 {
		body = overlayText(body, w, h, "(empty)")
	}
	return body
}

// renderScrolled renders the pane's scrollback view: the window of h rows
// ending scroll lines above the live bottom.
func renderScrolled(p *Pane, w, h, scroll int, sel viewSel) string {
	p.vt.Lock()
	defer p.vt.Unlock()

	cols, rows := p.vt.Size()
	total := len(p.scrollback) + rows
	top := total - scroll - h
	if top < 0 {
		top = 0
	}
	lines := make([][]vt10x.Glyph, 0, h)
	for y := 0; y < h; y++ {
		idx := top + y
		switch {
		case idx < 0 || idx >= total:
			lines = append(lines, nil)
		case idx < len(p.scrollback):
			lines = append(lines, p.scrollback[idx])
		default:
			lines = append(lines, vtRow(p.vt, idx-len(p.scrollback), cols))
		}
	}
	return renderGlyphs(lines, w, h, -1, -1, sel)
}

// renderPane renders one pane into exactly r.w × r.h cells: a thin rounded
// frame with the title embedded in the top border (`╭─ 1 · zsh ────╮`).
// Unfocused panes use a frame color close to the background to stay quiet.
func (m Model) renderPane(i int, r rect) string {
	return m.renderPaneFrame(i, r, false)
}

// renderMini renders a focus-mode sidebar mini: a live tail preview of the
// pane's trailing meaningful lines, bottom-aligned, never with a cursor. The
// focused (selected) mini is highlighted with the accent frame.
func (m Model) renderMini(i int, r rect) string {
	return m.renderPaneFrame(i, r, true)
}

func (m Model) renderPaneFrame(i int, r rect, mini bool) string {
	p := m.panes[i]
	focused := i == m.focus
	cw, ch := contentSize(r)

	// The mouse text selection lives in combined-buffer coordinates; convert
	// to view-line coordinates for this body's current window.
	sel := viewSel{}
	if !mini && m.selActive && m.selPane == i {
		x0, y0, x1, y1 := m.selX0, m.selY0, m.selX1, m.selY1
		if y0 > y1 || (y0 == y1 && x0 > x1) {
			x0, y0, x1, y1 = x1, y1, x0, y0
		}
		top := m.combinedTop(i, ch)
		sel = viewSel{active: true, ax: x0, ay: y0 - top, bx: x1, by: y1 - top}
	}

	var body string
	switch {
	case mini:
		body = renderMiniBody(p, cw, ch)
	case focused && m.scrolling && p.scroll > 0:
		body = renderScrolled(p, cw, ch, p.scroll, sel)
	default:
		// The terminal cursor is drawn only for the pane that owns the
		// keyboard: the focused pane in grid mode; the main view in focus
		// mode only when it owns the keyboard (EDIT). Hidden while editing
		// the title or during the blink-off phase.
		showCursor := focused && !m.editing && m.cursorOn && m.cursorAllowed()
		body = renderVT(p.vt, cw, ch, showCursor, sel)
	}

	frame := blurFrameStyle
	titleSt := blurTitleStyle
	if focused {
		frame = focusFrameStyle
		titleSt = focusTitleStyle
	}
	if !m.closeSelect {
		// Attention frames (yellow/red) light up panes needing a look.
		switch p.attention() {
		case attnUnread:
			frame = attnUnreadFrameStyle
		case attnNotify:
			frame = quitFrameStyle
		}
	}
	if m.closeSelect {
		if i == m.closeSel {
			// The highlighted close target gets the warning color.
			frame = quitFrameStyle
			titleSt = quitTitleStyle
		} else {
			// Every other pane lights up to show its index.
			frame = selectFrameStyle
			titleSt = selectTitleStyle
		}
	}

	var titleRendered string
	maxTitle := r.w - 9
	if maxTitle < 1 {
		maxTitle = 1
	}
	if focused && m.editing {
		titleRendered = titleSt.Render(m.titleInput.View())
	} else {
		titleRendered = m.renderTitleMeta(i, p, maxTitle, titleSt)
	}
	if ansi.StringWidth(titleRendered) > maxTitle {
		titleRendered = ansi.Truncate(titleRendered, maxTitle, "…")
	}
	// ╭─␣ + title + ␣dashes + ─ + ● + ─╮ : one dash on each side of the
	// dot for a more balanced look.
	dashes := r.w - 8 - ansi.StringWidth(titleRendered)
	if dashes < 1 {
		dashes = 1
	}
	top := frame.Render(frameTL+frameH+" ") + titleRendered + frame.Render(" "+strings.Repeat(frameH, dashes)+frameH) + m.renderDot(p) + frame.Render(frameH+frameTR)
	bottom := frame.Render(frameBL + strings.Repeat(frameH, r.w-2) + frameBR)

	var sb strings.Builder
	sb.WriteString(top)
	bodyLines := strings.Split(body, "\n")
	for y := 0; y < ch && y < r.h-2; y++ {
		sb.WriteByte('\n')
		line := ""
		if y < len(bodyLines) {
			line = bodyLines[y]
		}
		sb.WriteString(frame.Render(frameV) + line + frame.Render(frameV))
	}
	if r.h >= 2 {
		sb.WriteByte('\n')
		sb.WriteString(bottom)
	}
	return sb.String()
}

// buttonZone is the clickable column range [x0, x1) of a status bar button.
type buttonZone struct {
	id     string
	x0, x1 int
}

// statusBadges renders the mode badges (PREFIX / ZOOM / SCROLL), "" if none.
func (m Model) statusBadges() string {
	s := ""
	if m.leader {
		s += prefixBadgeStyle.Render(" PREFIX ")
	}
	if m.mode == modeFocus {
		if m.focusNav {
			s += navBadgeStyle.Render(" NAV ")
		} else {
			s += editBadgeStyle.Render(" EDIT ")
		}
	}
	if m.scrolling {
		s += scrollBadgeStyle.Render(" SCROLL ")
	}
	return s
}

func (m Model) statusModeName() string {
	if m.mode == modeFocus {
		if !m.sidebarLeft {
			return "focus·r"
		}
		return "focus"
	}
	return m.preset.String()
}

func (m Model) statusHints() string {
	if m.mode == modeFocus {
		if m.focusNav {
			return " ↑↓ switch · type to edit"
		}
		return " click mini to switch · type to edit"
	}
	return " F7 next-attn · dbl-click title=rename · F-keys on buttons"
}

// statusHintsShort is the fallback hint line for narrower windows.
func (m Model) statusHintsShort() string {
	if m.mode == modeFocus {
		return " ↑↓ switch · type edits"
	}
	return " F-keys on buttons"
}

// statusBarLayout computes the status bar line and the clickable button
// zones for width w. It is the single source of truth for both rendering
// and mouse hit-testing, so the two can never disagree.
//
// Narrow-window fallback: segments are dropped in a fixed priority order —
// hints, then the [layout] and [zoom] buttons, then the mode name — always
// keeping [+] [×] [mode] [quit]. As a last resort the line is truncated and
// zones outside the width are dropped, so it never renders broken.
func (m Model) statusBarLayout(w int) (string, []buttonZone) {
	modeAction := "grid"
	if m.mode == modeFocus {
		modeAction = "focus"
	}
	buttons := []struct{ id, key, action string }{
		{"new", "F2", "new"},
		{"close", "F3", "close"},
		{"mode", "F4", modeAction},
		{"layout", "F5", "layout"},
		{"dirs", "F6", "dirs"},
		{"notes", "F8", "notes"},
		{"quit", "F9", "quit"},
	}
	actionOf := func(id string) (string, string) {
		for _, b := range buttons {
			if b.id == id {
				return b.key, b.action
			}
		}
		return "?", "?"
	}
	renderBtn := func(id string) string {
		key, action := actionOf(id)
		return btnKeyStyle.Render(" "+key+" ") + btnActStyle.Render(action+" ")
	}
	btnWidth := func(id string) int {
		key, action := actionOf(id)
		return len(key) + len(action) + 3 // " F2 " + "new "
	}

	badges := m.statusBadges()
	nameFull := " " + m.statusModeName()
	nameShort := nameFull
	hintsFull := statusKeyStyle.Render(m.statusHints())
	hintsShort := statusKeyStyle.Render(m.statusHintsShort())
	if m.pendingRestore != nil {
		// The restore prompt takes the name slot (like the quit confirm).
		badges = quitBadgeStyle.Render(" RESTORE? ") + badges
		nameFull = quitTextStyle.Render(fmt.Sprintf(" restore previous session (%d terminals)? [y/n]", len(m.pendingRestore.Panes)))
		nameShort = nameFull
		hintsFull, hintsShort = "", ""
	}
	if m.confirmQuit {
		// The confirmation prompt takes the name slot (kept almost to the
		// end in the drop order) and the badge stays visible.
		badges = quitBadgeStyle.Render(" QUIT? ") + badges
		nameFull = quitTextStyle.Render(" quit all terminals? [y/n]")
		nameShort = nameFull
		hintsFull, hintsShort = "", ""
	}
	if m.notice != "" && !m.confirmQuit && !m.closeSelect {
		nameFull = statusKeyStyle.Render(" " + m.notice)
		nameShort = nameFull
	}
	if m.closeSelect && len(m.panes) > 0 {
		target := fmt.Sprintf("%d · %s", m.closeSel+1, m.panes[m.closeSel].title)
		badges = closeBadgeStyle.Render(" CLOSE? ") + badges
		nameFull = quitTextStyle.Render(" close which? (" + target + ") ↑↓←→ select · Enter confirm · click pane · Esc cancel")
		nameShort = quitTextStyle.Render(" close which? (" + target + ") Enter/Esc")
		hintsFull, hintsShort = "", ""
	}

	assemble := func(ids []string, nameLevel, hintsLevel int, compact bool) (string, []buttonZone, int) {
		var left strings.Builder
		var zones []buttonZone
		for i, id := range ids {
			if i > 0 && !compact {
				left.WriteString("  ")
			}
			if i > 0 && compact {
				left.WriteString(" ")
			}
			x0 := ansi.StringWidth(left.String())
			if compact {
				key, _ := actionOf(id)
				left.WriteString(btnKeyStyle.Render(" " + key + " "))
				zones = append(zones, buttonZone{id, x0, x0 + len(key) + 2})
			} else {
				left.WriteString(renderBtn(id))
				zones = append(zones, buttonZone{id, x0, x0 + btnWidth(id)})
			}
		}
		right := badges
		switch nameLevel {
		case 2:
			right = statusBarStyle.Render(nameFull) + right
		case 1:
			right = statusBarStyle.Render(nameShort) + right
		}
		switch hintsLevel {
		case 2:
			right += hintsFull
		case 1:
			right += hintsShort
		}
		gap := w - ansi.StringWidth(left.String()) - ansi.StringWidth(right)
		if gap < 0 {
			gap = 0
		}
		return left.String() + strings.Repeat(" ", gap) + right, zones, w - ansi.StringWidth(left.String()) - ansi.StringWidth(right)
	}

	all := []string{"new", "close", "mode", "layout", "dirs", "notes", "quit"}
	drop := func(ids []string, id string) []string {
		out := make([]string, 0, len(ids))
		for _, i := range ids {
			if i != id {
				out = append(out, i)
			}
		}
		return out
	}
	essential := []string{"new", "close", "mode", "quit"}
	// Narrow windows drop segments in this order: [notes] (least essential,
	// still reachable via F8 / leader N), full hints, short hints, [dirs],
	// [layout], full prompt/name, short prompt/name — always keeping
	// [+] [×] [mode] [quit] and the badges, with a key-only compact form as
	// the last step before hard truncation.
	noNotes := drop(all, "notes")
	attempts := []struct {
		ids              []string
		nameLevel, hints int
		compact          bool
	}{
		{all, 2, 2, false},
		{noNotes, 2, 2, false},
		{noNotes, 2, 1, false},
		{noNotes, 2, 0, false},
		{drop(noNotes, "dirs"), 2, 0, false},
		{drop(drop(noNotes, "dirs"), "layout"), 2, 0, false},
		{essential, 2, 0, false},
		{essential, 1, 0, false},
		{essential, 0, 0, false},
		{essential, 0, 0, true},
	}
	for _, a := range attempts {
		line, zones, gap := assemble(a.ids, a.nameLevel, a.hints, a.compact)
		if gap >= 0 {
			return line, zones
		}
	}

	// Last resort: compact essential buttons + badges, hard-truncated.
	line, zones, _ := assemble(essential, 0, 0, true)
	line = ansi.Truncate(line, w, "")
	kept := zones[:0]
	for _, z := range zones {
		if z.x1 <= w {
			kept = append(kept, z)
		}
	}
	return line, kept
}

// renderStatusBar renders the bottom action bar: clickable buttons on the
// left; mode badges, mode/layout name and key hints on the right. Quiet
// low-contrast text on the terminal's own background.
func (m Model) renderStatusBar() string {
	line, _ := m.statusBarLayout(m.width)
	return line
}

// dirsPickerRows is the maximum number of directory rows the picker shows.
const dirsPickerRows = 10

// dirsPickerLines renders the dirs picker (header + rows). Live entries
// (running shells' cwds) carry a ● marker; an empty list says so.
func (m Model) dirsPickerLines() []string {
	lines := []string{statusKeyStyle.Render(" " + iconDirs + "terminal dirs ─ enter to open · esc to cancel")}
	n := len(m.dirs)
	if n > dirsPickerRows {
		n = dirsPickerRows
	}
	if n == 0 {
		return append(lines, blurTitleStyle.Render("   no recent dirs"))
	}
	for i := 0; i < n; i++ {
		d := m.dirs[i]
		label := d.path
		if d.live {
			label = "● " + d.path
		}
		if i == m.dirsSel {
			lines = append(lines, focusTitleStyle.Render(" ▸ "+label))
		} else {
			lines = append(lines, blurTitleStyle.Render("   "+label))
		}
	}
	return lines
}

// dirsOverlayGeometry returns the picker's top row within the content area
// and its rendered lines (shared by the View overlay and mouse hit-testing).
func (m Model) dirsOverlayGeometry() (startY int, lines []string) {
	_, h := m.contentArea()
	lines = m.dirsPickerLines()
	startY = h - len(lines)
	if startY < 0 {
		startY = 0
	}
	return startY, lines
}

// overlayDirs draws the picker as a bottom drawer over the pane area.
func (m Model) overlayDirs(body string) string {
	startY, lines := m.dirsOverlayGeometry()
	return m.overlayDrawer(body, lines, startY)
}

// notesPickerRows is the maximum number of entries the drawer shows.
const notesPickerRows = 10

// notesPickerLines renders the notification history drawer (header + rows),
// newest first.
func (m Model) notesPickerLines() []string {
	lines := []string{statusKeyStyle.Render(" " + iconNotes + "notifications ─ enter to jump · esc to cancel")}
	n := len(m.notes)
	if n > notesPickerRows {
		n = notesPickerRows
	}
	for i := 0; i < n; i++ {
		e := m.notes[i]
		title := "?"
		if e.pane >= 0 && e.pane < len(m.panes) {
			title = m.panes[e.pane].title
		}
		label := fmt.Sprintf("%s · %d · %s · %s", e.at.Format("15:04"), e.pane+1, title, e.text)
		if i == m.notesSel {
			lines = append(lines, focusTitleStyle.Render(" ▸ "+label))
		} else {
			lines = append(lines, blurTitleStyle.Render("   "+label))
		}
	}
	return lines
}

// notesOverlayGeometry mirrors dirsOverlayGeometry for the notes drawer.
func (m Model) notesOverlayGeometry() (startY int, lines []string) {
	_, h := m.contentArea()
	lines = m.notesPickerLines()
	startY = h - len(lines)
	if startY < 0 {
		startY = 0
	}
	return startY, lines
}

// overlayNotes draws the notification history as a bottom drawer.
func (m Model) overlayNotes(body string) string {
	startY, lines := m.notesOverlayGeometry()
	return m.overlayDrawer(body, lines, startY)
}

// overlayDrawer draws lines as a bottom drawer over the pane area, starting
// at row startY, padding/truncating each line to the content width.
func (m Model) overlayDrawer(body string, lines []string, startY int) string {
	w, _ := m.contentArea()
	bodyLines := strings.Split(body, "\n")
	for i, pl := range lines {
		y := startY + i
		if y >= len(bodyLines) {
			break
		}
		switch dw := ansi.StringWidth(pl); {
		case dw < w:
			pl += strings.Repeat(" ", w-dw)
		case dw > w:
			pl = ansi.Truncate(pl, w, "")
		}
		bodyLines[y] = pl
	}
	return strings.Join(bodyLines, "\n")
}

// renderMiniIndicator renders one sidebar scroll-indicator row
// ("↑ N more" / "↓ N more", dim), padded to w cells.
func renderMiniIndicator(up bool, count, w int) string {
	text := "   "
	if count > 0 {
		arrow := "↓"
		if up {
			arrow = "↑"
		}
		text = fmt.Sprintf(" %s %d more", arrow, count)
	}
	text = statusKeyStyle.Render(text)
	if dw := ansi.StringWidth(text); dw < w {
		text += strings.Repeat(" ", w-dw)
	} else if dw > w {
		text = ansi.Truncate(text, w, "")
	}
	return text
}

// renderDot renders the pane's attention dot: a filled circle, legible at a
// glance even in peripheral vision.
func (m Model) renderDot(p *Pane) string {
	st := lipgloss.NewStyle().Foreground(dotIdleColor)
	switch p.attention() {
	case attnActive:
		st = lipgloss.NewStyle().Foreground(dotActiveColor)
	case attnUnread:
		st = lipgloss.NewStyle().Foreground(dotUnreadColor)
	case attnNotify:
		st = lipgloss.NewStyle().Foreground(quitColor)
	}
	return st.Render("●")
}

// renderTitleMeta composes the pane title with metadata: "N · title ·
// branch · proc" where the agent name (carrying a Nerd Font icon) is
// highlighted. Segments are dropped in reverse priority when space is
// tight (branch first, then proc).
func (m Model) renderTitleMeta(i int, p *Pane, maxW int, baseStyle lipgloss.Style) string {
	p.stateMu.Lock()
	proc, branch, agent := p.proc, p.branch, p.agent
	p.stateMu.Unlock()

	plain := fmt.Sprintf("%d · %s", i+1, p.title)
	compose := func(withBranch, withProc bool) (string, int) {
		s := baseStyle.Render(plain)
		w := ansi.StringWidth(plain)
		if withBranch && branch != "" {
			s += statusBarStyle.Render(" · " + branch)
			w += 3 + ansi.StringWidth(branch)
		}
		if withProc && (agent != "" || proc != "") {
			t, st := proc, statusBarStyle
			if agent != "" {
				t, st = iconAgent+agent, agentNameStyle
			}
			s += st.Render(" · " + t)
			w += 3 + ansi.StringWidth(t)
		}
		return s, w
	}
	if s, w := compose(true, true); w <= maxW {
		return s
	}
	if s, w := compose(false, true); w <= maxW {
		return s
	}
	if s, w := compose(false, false); w <= maxW {
		return s
	}
	return ansi.Truncate(baseStyle.Render(plain), maxW, "…")
}

// cursorAllowed reports whether the terminal cursor should be drawn for the
// focused pane: always in grid mode; in focus mode only when the main view
// owns the keyboard (EDIT ownership).
func (m Model) cursorAllowed() bool {
	return m.mode != modeFocus || !m.focusNav
}
