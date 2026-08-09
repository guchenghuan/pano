package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Session resurrection: on quit the layout (split tree + ratios), per-pane
// title and cwd, and the focus are saved to ~/.config/pano/session.json
// ($XDG_CONFIG_HOME respected). On startup a saved session is offered as a
// [y/n] prompt; confirming recreates the panes (fresh shells in their old
// directories — running programs are never auto-rerun), declining discards
// the file. Closing the last pane deletes the file instead of saving an
// empty session.

// sessionPane is the saved state of one pane (creation order).
type sessionPane struct {
	Title string `json:"title"`
	Dir   string `json:"dir"`
}

// sessionNode is the serializable mirror of the split tree. Leaves carry a
// Pane index into sessionFile.Panes; internal nodes carry Dir ("h"/"v") and
// Ratio. Pane is -1 on internal nodes.
type sessionNode struct {
	Dir   string       `json:"dir,omitempty"`
	Ratio float64      `json:"ratio,omitempty"`
	A     *sessionNode `json:"a,omitempty"`
	B     *sessionNode `json:"b,omitempty"`
	Pane  int          `json:"pane"`
}

// sessionFile is the on-disk session snapshot.
type sessionFile struct {
	SavedAt   time.Time     `json:"savedAt"`
	Preset    string        `json:"preset"`
	MainRatio float64       `json:"mainRatio"`
	Focus     int           `json:"focus"`
	Panes     []sessionPane `json:"panes"`
	Root      *sessionNode  `json:"root,omitempty"`
}

// sessionPath locates the session snapshot file ("" when unknown).
func sessionPath() string {
	if d := configDir(); d != "" {
		return filepath.Join(d, "session.json")
	}
	return ""
}

// sessionSnapshot captures the current layout and pane metadata. Returns nil
// when no pane is alive (caller then removes any stale session file).
func (m *Model) sessionSnapshot() *sessionFile {
	if len(m.panes) == 0 {
		return nil
	}
	idx := make(map[*Pane]int, len(m.panes))
	panes := make([]sessionPane, len(m.panes))
	for i, p := range m.panes {
		idx[p] = i
		dir := ""
		if p.pid != 0 {
			dir = procCWD(p.pid)
		}
		panes[i] = sessionPane{Title: p.title, Dir: dir}
	}
	var enc func(n *node) *sessionNode
	enc = func(n *node) *sessionNode {
		if n == nil {
			return nil
		}
		if n.pane != nil {
			return &sessionNode{Pane: idx[n.pane]}
		}
		d := "h"
		if n.dir == splitV {
			d = "v"
		}
		return &sessionNode{Dir: d, Ratio: n.ratio, A: enc(n.a), B: enc(n.b), Pane: -1}
	}
	mainRatio := m.mainRatio
	if mainRatio <= 0 {
		mainRatio = defaultMain
	}
	return &sessionFile{
		SavedAt:   time.Now(),
		Preset:    m.preset.String(),
		MainRatio: mainRatio,
		Focus:     m.focus,
		Panes:     panes,
		Root:      enc(m.root),
	}
}

// writeSession saves the snapshot (best effort; callers ignore the error).
func writeSession(path string, sf *sessionFile) error {
	data, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// loadSession reads a saved session; a missing file is (nil, nil), a
// malformed one is (nil, err).
func loadSession(path string) (*sessionFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sf sessionFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, err
	}
	if len(sf.Panes) == 0 {
		return nil, nil
	}
	return &sf, nil
}

// buildSessionTree rebuilds the split tree from its serialized form. Any
// inconsistency (bad index, missing child, pane left unreferenced) yields
// nil so the caller can fall back to a preset tree.
func buildSessionTree(panes []*Pane, sn *sessionNode) *node {
	used := make([]bool, len(panes))
	var build func(sn *sessionNode) *node
	build = func(sn *sessionNode) *node {
		if sn == nil {
			return nil
		}
		if sn.Pane >= 0 {
			if sn.Pane >= len(panes) || used[sn.Pane] {
				return nil
			}
			used[sn.Pane] = true
			return leaf(panes[sn.Pane])
		}
		if sn.A == nil || sn.B == nil {
			return nil
		}
		a, b := build(sn.A), build(sn.B)
		if a == nil || b == nil {
			return nil
		}
		dir := splitH
		if sn.Dir == "v" {
			dir = splitV
		}
		return &node{dir: dir, ratio: clampf(sn.Ratio, minRatio, maxRatio), a: a, b: b}
	}
	root := build(sn)
	if root == nil {
		return nil
	}
	for _, u := range used {
		if !u {
			return nil
		}
	}
	return root
}

// restoreSession recreates the saved session: fresh shells in their old
// directories with their old titles, the saved tree shape and focus.
// Directories that no longer open cleanly fall back to the default cwd.
func (m *Model) restoreSession(sf *sessionFile) {
	for _, sp := range sf.Panes {
		dir := sp.Dir
		if dir != "" && !dirExists(dir) {
			dir = ""
		}
		m.addPane(dir)
		if sp.Title != "" {
			m.panes[len(m.panes)-1].title = sp.Title
		}
	}
	if len(m.panes) == 0 {
		return
	}
	if sf.Preset == presetMainLeft.String() {
		m.preset = presetMainLeft
	} else {
		m.preset = presetEvenGrid
	}
	if sf.MainRatio > 0 {
		m.mainRatio = clampf(sf.MainRatio, minRatio, maxRatio)
	}
	m.root = buildSessionTree(m.panes, sf.Root)
	if m.root == nil {
		m.root = buildTree(m.panes, m.preset, m.mainRatio)
	}
	m.focus = max(0, min(sf.Focus, len(m.panes)-1))
	m.gridFocus = m.focus
	m.layoutPanes()
}
