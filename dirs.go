package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// dirEntry is one row of the dirs picker: a directory the user's terminals
// are in right now (live) or have cd'ed to before (from zsh history).
type dirEntry struct {
	path string
	live bool // true = a currently running shell's cwd (marked ●)
}

const recentDirsMax = 20

// System probes are package-level variables so tests can inject fakes.
var (
	shellPIDs = defaultShellPIDs     // pids of running interactive shells
	procCWD   = defaultProcCWD       // cwd of a pid ("" on failure)
	dirExists = defaultDirExists     // directory existence check
)

// defaultShellPIDs lists pids of running zsh/bash/fish processes. All
// failures are ignored (missing pgrep, permission errors, etc).
func defaultShellPIDs() []int {
	var out []int
	for _, sh := range []string{"zsh", "bash", "fish"} {
		data, err := exec.Command("pgrep", "-x", sh).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}

// defaultProcCWD asks lsof for the process's current working directory.
func defaultProcCWD(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimSpace(line[1:])
		}
	}
	return ""
}

func defaultDirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// liveShellDirs returns the cwds of running shells, excluding selfPIDs
// (pano's own pane shells), deduplicated.
func liveShellDirs(exclude map[int]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, pid := range shellPIDs() {
		if exclude[pid] {
			continue
		}
		if d := procCWD(pid); d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// histfilePath resolves the zsh history file: $HISTFILE if set, otherwise
// ~/.zsh_history.
func histfilePath() string {
	if hf := os.Getenv("HISTFILE"); hf != "" {
		return hf
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".zsh_history")
	}
	return ""
}

// parseZshHistory simulates cwd movement through a zsh history file and
// returns visited directories, most recently used first, deduplicated.
// Both the extended format (`: 1700000000:0;cd /path`) and plain lines
// (`cd /path`) are supported.
func parseZshHistory(path, home string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	cwd := home
	lastVisit := map[string]int{} // dir -> index of its most recent visit
	order := 0
	for _, line := range strings.Split(string(data), "\n") {
		cmd := strings.TrimSpace(line)
		// Strip the extended-history prefix `: <ts>:<dur>;`.
		if strings.HasPrefix(cmd, ": ") {
			if i := strings.Index(cmd, ";"); i >= 0 {
				cmd = cmd[i+1:]
			}
		}
		fields := strings.Fields(cmd)
		if len(fields) == 0 || (fields[0] != "cd" && fields[0] != "pushd") {
			continue
		}
		target := ""
		for _, a := range fields[1:] {
			if strings.HasPrefix(a, "-") { // skip options (incl. `cd -`)
				if a == "-" {
					target = "-"
				}
				continue
			}
			target = a
			break
		}
		switch {
		case target == "-":
			continue // cd - : ignore
		case target == "":
			target = home // bare cd goes home
		default:
			target = unquote(target)
			if target == "~" {
				target = home
			} else if strings.HasPrefix(target, "~/") {
				target = filepath.Join(home, target[2:])
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(cwd, target)
			}
			target = filepath.Clean(target)
		}
		cwd = target
		lastVisit[target] = order
		order++
	}
	// Most recently visited first.
	dirs := make([]string, 0, len(lastVisit))
	for d := range lastVisit {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(a, b int) bool { return lastVisit[dirs[a]] > lastVisit[dirs[b]] })
	return dirs
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// collectDirs computes the picker list fresh on every open: running shells'
// cwds first (marked live), then zsh-history directories by recency; merged,
// deduplicated, non-existent directories filtered, capped.
func collectDirs(exclude map[int]bool) []dirEntry {
	seen := map[string]bool{}
	var out []dirEntry
	add := func(path string, live bool) {
		if path == "" || seen[path] || !dirExists(path) {
			return
		}
		seen[path] = true
		out = append(out, dirEntry{path: path, live: live})
	}
	for _, d := range liveShellDirs(exclude) {
		add(d, true)
	}
	home, _ := os.UserHomeDir()
	for _, d := range parseZshHistory(histfilePath(), home) {
		add(d, false)
	}
	if len(out) > recentDirsMax {
		out = out[:recentDirsMax]
	}
	return out
}
