package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// The agent process-name set lives in agentCfg.names (agents.go), loaded
// from the user config with built-in defaults.

// System probes, injectable for tests.
var psSnapshot = defaultPsSnapshot // process table: pid -> (ppid, comm)

type psEntry struct {
	ppid int
	comm string
}

// defaultPsSnapshot parses `ps -eo pid,ppid,comm` in one call.
func defaultPsSnapshot() map[int]psEntry {
	out, err := exec.Command("ps", "-eo", "pid,ppid,comm").Output()
	if err != nil {
		return nil
	}
	table := map[int]psEntry{}
	for i, line := range strings.Split(string(out), "\n") {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		table[pid] = psEntry{ppid: ppid, comm: filepath.Base(fields[2])}
	}
	return table
}

// commChain returns the command names along the path from root down to its
// deepest descendant (root first). Just root's own name when childless.
func commChain(table map[int]psEntry, root int) []string {
	children := map[int][]int{}
	for pid, e := range table {
		children[e.ppid] = append(children[e.ppid], pid)
	}
	// Descend to the deepest leaf, recording the path.
	path := []int{root}
	cur := root
	for {
		kids := children[cur]
		if len(kids) == 0 {
			break
		}
		cur = kids[0]
		path = append(path, cur)
	}
	out := make([]string, 0, len(path))
	for _, pid := range path {
		out = append(out, table[pid].comm)
	}
	return out
}

// deepestComm walks down the process tree from root to its deepest
// descendant (longest chain wins; ties broken arbitrarily) and returns its
// command name, or "" when unknown.
func deepestComm(table map[int]psEntry, root int) string {
	children := map[int][]int{}
	for pid, e := range table {
		children[e.ppid] = append(children[e.ppid], pid)
	}
	best, bestDepth := root, 0
	var walk func(pid, depth int)
	walk = func(pid, depth int) {
		if depth > bestDepth {
			best, bestDepth = pid, depth
		}
		for _, c := range children[pid] {
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	if best == root {
		if e, ok := table[root]; ok {
			return e.comm
		}
		return ""
	}
	return table[best].comm
}

// gitBranch resolves the git branch of dir by reading .git/HEAD directly
// (no git exec), handling worktrees where .git is a gitdir file. "" when
// not in a repo.
func gitBranch(dir string) string {
	for i := 0; i < 16 && dir != "." && dir != "/"; i++ {
		dotgit := filepath.Join(dir, ".git")
		if st, err := os.Stat(dotgit); err == nil {
			if st.IsDir() {
				return readHEAD(filepath.Join(dotgit, "HEAD"))
			}
			// Worktree: .git is a file "gitdir: <path>".
			if data, err := os.ReadFile(dotgit); err == nil {
				line := strings.TrimSpace(string(data))
				if rest, ok := strings.CutPrefix(line, "gitdir: "); ok {
					return readHEAD(filepath.Join(strings.TrimSpace(rest), "HEAD"))
				}
			}
			return ""
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func readHEAD(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))
	if rest, ok := strings.CutPrefix(head, "ref: refs/heads/"); ok {
		return rest
	}
	if len(head) > 7 { // detached: short sha
		return head[:7]
	}
	return ""
}

// refreshMeta is the periodic (tick) collector: foreground process name and
// git branch for every pane, using one ps call for all panes. The agent
// name is the deepest agent-named process in the shell's subtree (so
// `claude … sleep` still resolves to claude).
func (m *Model) refreshMeta() {
	table := psSnapshot()
	shells := map[string]bool{"zsh": true, "bash": true, "fish": true, "sh": true}
	for _, p := range m.panes {
		if p.pid == 0 {
			continue
		}
		chain := commChain(table, p.pid)
		proc, agent := "", ""
		for _, c := range chain {
			if agentCfg.names[c] {
				agent = c
			}
		}
		if len(chain) > 0 {
			proc = chain[len(chain)-1]
			if shells[proc] || proc == agent { // idle shell or the agent itself
				proc = ""
			}
		}
		branch := ""
		if cwd := procCWD(p.pid); cwd != "" {
			branch = gitBranch(cwd)
		}
		p.setMeta(proc, branch, agent)
	}
}
