package main

import (
	"math"
	"sort"
)

// rect is a rectangular region of the screen in cell coordinates.
type rect struct {
	x, y, w, h int
}

// preset identifies a layout preset (the initial shape of the split tree).
type preset int

const (
	presetEvenGrid preset = iota // balanced grid
	presetMainLeft               // pane 0 large on the left, rest stacked right
	presetCount
)

func (p preset) String() string {
	switch p {
	case presetMainLeft:
		return "main-left"
	default:
		return "even-grid"
	}
}

const (
	defaultMain  = 0.65 // default main-pane share for main-left
	weightStep   = 0.05 // keyboard ratio adjustment step
	minRatio     = 0.1  // split ratio clamp
	maxRatio     = 0.9
	sidebarWidth = 26 // default width of the focus-mode sidebar (adjustable)
	minSidebarW  = 16 // keeps mini titles readable
)

// clampSidebarW clamps the sidebar width: at least minSidebarW, at most
// half the window, and always leaving the main view ≥ 30 cells.
func clampSidebarW(v, w int) int {
	maxW := w - 30
	if half := w / 2; half < maxW {
		maxW = half
	}
	if maxW < minSidebarW {
		maxW = minSidebarW
	}
	return max(minSidebarW, min(v, maxW))
}

// splitDir is the orientation of an internal split node.
type splitDir int

const (
	splitH splitDir = iota // children side by side (a left, b right)
	splitV                 // children stacked (a top, b bottom)
)

// node is a node of the split tree. Leaves hold a pane; internal nodes split
// their rect between children a (ratio share) and b.
type node struct {
	dir   splitDir
	ratio float64
	a, b  *node
	pane  *Pane // non-nil only on leaves
}

func leaf(p *Pane) *node { return &node{pane: p} }

// gridDims returns the column/row counts for n panes:
// cols = ceil(sqrt(n)), rows = ceil(n/cols).
func gridDims(n int) (cols, rows int) {
	if n <= 0 {
		return 0, 0
	}
	cols = int(math.Ceil(math.Sqrt(float64(n))))
	rows = (n + cols - 1) / cols
	return cols, rows
}

// chain builds a right-leaning split chain giving each leaf an equal share.
func chain(dir splitDir, leaves []*node) *node {
	if len(leaves) == 1 {
		return leaves[0]
	}
	return &node{
		dir:   dir,
		ratio: 1.0 / float64(len(leaves)),
		a:     leaves[0],
		b:     chain(dir, leaves[1:]),
	}
}

// evenGridCounts picks the column count and per-row pane counts for the
// even grid. Candidates whose max/min pane area ratio reaches 2 (slivers or
// oversized spans) are rejected; among the rest the column count closest to
// ceil(sqrt(n)) wins, ties preferring wider grids.
func evenGridCounts(n int) (cols int, counts []int) {
	if n <= 0 {
		return 0, nil
	}
	refCols, _ := gridDims(n)
	ref := float64(refCols)
	bestCols, bestCounts := n, []int{n}
	bestDist := math.MaxFloat64
	for c := 1; c <= n; c++ {
		rows := (n + c - 1) / c
		rc := distribute(n, normalizeWeights(nil, rows)) // ints differ ≤ 1
		minA, maxA := math.MaxFloat64, 0.0
		for _, cnt := range rc {
			a := 1.0 / float64(cnt*rows) // area ∝ (1/count) × (1/rows)
			if a < minA {
				minA = a
			}
			if a > maxA {
				maxA = a
			}
		}
		if maxA >= 2*minA {
			continue // too skewed (e.g. a lone pane spanning a full row)
		}
		if dist := math.Abs(float64(c) - ref); dist < bestDist-1e-9 ||
			(math.Abs(dist-bestDist) < 1e-9 && c > bestCols) {
			bestDist, bestCols, bestCounts = dist, c, rc
		}
	}
	return bestCols, bestCounts
}

// buildTree builds the initial split tree for a preset. Pane order is
// preserved left-to-right, top-to-bottom.
func buildTree(panes []*Pane, p preset, mainRatio float64) *node {
	if len(panes) == 0 {
		return nil
	}
	leaves := make([]*node, len(panes))
	for i, pn := range panes {
		leaves[i] = leaf(pn)
	}
	if len(leaves) == 1 {
		return leaves[0]
	}
	switch p {
	case presetMainLeft:
		return &node{dir: splitH, ratio: mainRatio, a: leaves[0], b: chain(splitV, leaves[1:])}
	default: // even grid: balanced rows of horizontal chains
		_, counts := evenGridCounts(len(leaves))
		rowNodes := make([]*node, 0, len(counts))
		i := 0
		for _, cnt := range counts {
			rowNodes = append(rowNodes, chain(splitH, leaves[i:i+cnt]))
			i += cnt
		}
		return chain(splitV, rowNodes)
	}
}

// splitLeaf replaces leaf l in place with a split of l and nl: l keeps its
// half, nl gets the other. dir picks the orientation.
func splitLeaf(l, nl *node, dir splitDir) {
	old := *l
	l.pane = nil
	l.dir = dir
	l.ratio = 0.5
	l.a = &old
	l.b = nl
}

// removeLeaf collapses l's parent split so the sibling subtree takes over
// the space. Returns the new root (nil when the last leaf was removed).
func removeLeaf(root, l *node) *node {
	if root == l {
		return nil
	}
	var walk func(n *node) bool
	walk = func(n *node) bool {
		if n == nil || n.pane != nil {
			return false
		}
		if n.a == l {
			*n = *n.b // sibling subtree takes over
			return true
		}
		if n.b == l {
			*n = *n.a
			return true
		}
		return walk(n.a) || walk(n.b)
	}
	walk(root)
	return root
}

// layoutNode assigns rects to every node: leaves by pane, internal nodes by
// pointer (either map may be nil).
func layoutNode(n *node, r rect, leaves map[*Pane]rect, nodes map[*node]rect) {
	if n == nil {
		return
	}
	if nodes != nil {
		nodes[n] = r
	}
	if n.pane != nil {
		if leaves != nil {
			leaves[n.pane] = r
		}
		return
	}
	if n.dir == splitH {
		aw := int(math.Round(float64(r.w) * n.ratio))
		aw = max(1, min(aw, r.w-1))
		layoutNode(n.a, rect{r.x, r.y, aw, r.h}, leaves, nodes)
		layoutNode(n.b, rect{r.x + aw, r.y, r.w - aw, r.h}, leaves, nodes)
		return
	}
	ah := int(math.Round(float64(r.h) * n.ratio))
	ah = max(1, min(ah, r.h-1))
	layoutNode(n.a, rect{r.x, r.y, r.w, ah}, leaves, nodes)
	layoutNode(n.b, rect{r.x, r.y + ah, r.w, r.h - ah}, leaves, nodes)
}

// subtreeHas reports whether pane p lives under n.
func subtreeHas(n *node, p *Pane) bool {
	if n == nil {
		return false
	}
	if n.pane != nil {
		return n.pane == p
	}
	return subtreeHas(n.a, p) || subtreeHas(n.b, p)
}

// leafPath returns the path from root down to the leaf holding p
// (root first, leaf last), or nil if p is not in the tree.
func leafPath(root *node, p *Pane) []*node {
	if root == nil {
		return nil
	}
	if root.pane != nil {
		if root.pane == p {
			return []*node{root}
		}
		return nil
	}
	if path := leafPath(root.a, p); path != nil {
		return append([]*node{root}, path...)
	}
	if path := leafPath(root.b, p); path != nil {
		return append([]*node{root}, path...)
	}
	return nil
}

// findSplitNode returns the deepest ancestor of a's leaf that has the given
// orientation and separates a from b (a and b in different subtrees). This
// is the node whose ratio a drag of the a|b boundary should adjust.
func findSplitNode(root *node, a, b *Pane, dir splitDir) *node {
	path := leafPath(root, a)
	for i := len(path) - 1; i >= 0; i-- {
		nd := path[i]
		if nd.pane != nil || nd.dir != dir {
			continue
		}
		if subtreeHas(nd.a, a) != subtreeHas(nd.a, b) {
			return nd
		}
	}
	return nil
}

// nudgeSplit adjusts the ratio of the nearest ancestor split of pane p with
// the given orientation by delta (keyboard HJKL resizing). The sign is
// normalized so a positive delta always grows p's share.
func nudgeSplit(root *node, p *Pane, dir splitDir, delta float64) {
	path := leafPath(root, p)
	for i := len(path) - 2; i >= 0; i-- { // skip the leaf itself
		nd := path[i]
		if nd.pane != nil || nd.dir != dir {
			continue
		}
		if !subtreeHas(nd.a, p) {
			delta = -delta
		}
		nd.ratio = clampf(nd.ratio+delta, minRatio, maxRatio)
		return
	}
}

// promoteRatio is the share the clicked pane's side gets in every split on
// its path from the root (click-promote).
const promoteRatio = 0.62

// promotePane raises the clicked pane's share to promoteRatio along its
// ancestor splits — but only ever *raises*: a side that already has ≥
// promoteRatio (e.g. manually dragged wider) is left untouched, so custom
// proportions elsewhere in the tree survive a click.
func promotePane(root *node, p *Pane) {
	for _, nd := range leafPath(root, p) {
		if nd.pane != nil {
			continue
		}
		if subtreeHas(nd.a, p) {
			nd.ratio = max(nd.ratio, promoteRatio) // p in a: raise a's share
		} else {
			nd.ratio = min(nd.ratio, 1-promoteRatio) // p in b: raise b's share
		}
	}
}
// distribute splits total cells proportionally to weights (largest remainder
// method), so the parts sum exactly to total. When total >= len(weights),
// every part gets at least 1 cell.
func distribute(total int, weights []float64) []int {
	n := len(weights)
	out := make([]int, n)
	if n == 0 || total <= 0 {
		return out
	}
	if total >= n {
		extra := distributeRaw(total-n, weights)
		for i := range out {
			out[i] = 1 + extra[i]
		}
		return out
	}
	return distributeRaw(total, weights)
}

// distributeRaw splits total proportionally without a minimum guarantee.
func distributeRaw(total int, weights []float64) []int {
	n := len(weights)
	out := make([]int, n)
	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	type frac struct {
		i int
		f float64
	}
	fr := make([]frac, n)
	acc := 0
	for i, w := range weights {
		exact := float64(total) * w / sum
		out[i] = int(math.Floor(exact))
		fr[i] = frac{i, exact - math.Floor(exact)}
		acc += out[i]
	}
	sort.Slice(fr, func(a, b int) bool { return fr[a].f > fr[b].f })
	for k := 0; acc < total; k++ {
		out[fr[k%n].i]++
		acc++
	}
	return out
}

func normalizeWeights(w []float64, n int) []float64 {
	if len(w) == n {
		return w
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

func clampf(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// focusRects computes the focus-mode layout: all panes are stacked as minis
// in a narrow sidebar (minis, one per pane), and the focused pane also fills
// the main view (main). The two regions tile the w×h area.
// miniMinH is the minimum height of a sidebar mini (border included): the
// title row plus a couple of content lines. When the minis no longer fit,
// they keep this height and the list scrolls instead of shrinking further.
const miniMinH = 5

// miniVisibleCount reports how many minis fit in height h and whether the
// list overflows (in which case one indicator row is reserved at the top
// and one at the bottom).
func miniVisibleCount(n, h int) (visible int, overflowing bool) {
	if n*miniMinH <= h {
		return n, false
	}
	visible = (h - 2) / miniMinH
	if visible < 1 {
		visible = 1
	}
	return visible, true
}

// focusLayout computes the focus-mode layout. Minis are evenly stretched
// when they all fit; otherwise they keep miniMinH and a window of `visible`
// minis starting at `offset` is shown (indicator rows at the very top and
// bottom of the sidebar). Invisible minis get the zero rect (never rendered
// or hit-tested). Returns the minis, the main view rect, and the hidden
// counts above/below the window.
func focusLayout(n, w, h, focus, offset, sideW int, sidebarLeft bool) (minis []rect, main rect, moreUp, moreDown int) {
	minis = make([]rect, n)
	if n == 1 {
		// A single pane needs no sidebar; the zero-sized mini is skipped by
		// the renderer and never hit-tested.
		return minis, rect{0, 0, w, h}, 0, 0
	}
	if sideW > w/2 {
		sideW = w / 2
	}
	if sideW < 1 {
		sideW = 1
	}
	sideX := 0
	mainX := sideW
	if !sidebarLeft {
		sideX = w - sideW
		mainX = 0
	}
	main = rect{x: mainX, y: 0, w: w - sideW, h: h}

	visible, overflowing := miniVisibleCount(n, h)
	if !overflowing {
		heights := distribute(h, normalizeWeights(nil, n))
		y := 0
		for i := 0; i < n; i++ {
			minis[i] = rect{x: sideX, y: y, w: sideW, h: heights[i]}
			y += heights[i]
		}
		return minis, main, 0, 0
	}

	offset = max(0, min(offset, n-visible))
	y := 1 // below the top indicator row
	shown := 0
	for i := offset; i < n && shown < visible; i++ {
		minis[i] = rect{x: sideX, y: y, w: sideW, h: miniMinH}
		y += miniMinH
		shown++
	}
	// The last visible mini stretches down to the bottom indicator row.
	last := offset + shown - 1
	minis[last].h = (h - 1) - minis[last].y
	return minis, main, offset, n - offset - shown
}

// boundary is a draggable border between two adjacent panes. vertical=true
// means a vertical border (dragging adjusts widths).
type boundary struct {
	a, b     int
	vertical bool
}

// hitSlack is how many cells beyond the two border columns still count as
// grabbing the boundary (on each side).
const hitSlack = 1

// findBoundaries returns all draggable borders between adjacent panes at
// screen position (x, y). A corner where four panes meet can return both a
// vertical and a horizontal boundary, enabling two-dimensional corner drags.
// Rects are adjacent (a's right/bottom edge touches b's left/top edge), and
// the hit zone is the two border columns/rows plus hitSlack on each side.
func findBoundaries(rects []rect, x, y int) []boundary {
	var out []boundary
	for i := range rects {
		for j := range rects {
			if i == j {
				continue
			}
			ra, rb := rects[i], rects[j]
			// i left of j, sharing a vertical border.
			if ra.x+ra.w == rb.x &&
				x >= ra.x+ra.w-1-hitSlack && x <= rb.x+hitSlack &&
				y >= max(ra.y, rb.y)-hitSlack && y < min(ra.y+ra.h, rb.y+rb.h)+hitSlack {
				out = append(out, boundary{i, j, true})
			}
			// i above j, sharing a horizontal border.
			if ra.y+ra.h == rb.y &&
				y >= ra.y+ra.h-1-hitSlack && y <= rb.y+hitSlack &&
				x >= max(ra.x, rb.x)-hitSlack && x < min(ra.x+ra.w, rb.x+rb.w)+hitSlack {
				out = append(out, boundary{i, j, false})
			}
		}
	}
	return out
}

// contentSize returns the terminal content size (pty/vt size) for a pane
// occupying r: minus 1 border column and 1 border row on each side (the
// title is embedded in the top border, it does not take an extra row).
func contentSize(r rect) (w, h int) {
	w = r.w - 2
	h = r.h - 2
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h
}
