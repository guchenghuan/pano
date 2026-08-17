package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// External control channel (zellij-pipe style): each pane's shell gets
// PANO_SOCK/PANO_PANE in its environment, and anything running inside a pane
// (agent hooks, scripts) can drive pano with `pano ctl …`:
//
//	pano ctl notify "Build finished"   # red-dot the pane + history entry
//	pano ctl notify                    # same, bell-style (no text)
//	pano ctl focus                     # focus the pane
//	pano ctl send 2 "tests green"      # red-dot another pane (index|title|all)
//	pano ctl type api "make test"      # type a command into another pane
//	pano ctl watch build [--type]      # subscribe this pane to a channel
//	pano ctl emit build "build done"   # deliver to every subscriber
//
// The socket is per-instance (pid-named, mode 0600), removed on exit.
// Commands are single text lines "<pane-id> <command>"; the pane id names
// the sender, send/type/emit carry their own target selector.

// ctlMsg is one command received over the control socket.
type ctlMsg struct {
	paneID int
	kind   string // "notify", "focus", "send", "type", "watch", "unwatch" or "emit"
	target string // send/type: pane selector; watch/unwatch/emit: channel name
	text   string
	typed  bool // watch: deliver emitted text as typed input instead of a red dot
}

const (
	envCtlSock = "PANO_SOCK"
	envCtlPane = "PANO_PANE"
)

// ctlSocketPath is this instance's control socket ("" = channel disabled,
// e.g. the listen failed); read by newPane when building the shell env.
var ctlSocketPath string

// defaultCtlSocketPath picks a per-instance socket path in the temp dir.
func defaultCtlSocketPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("pano-%d.sock", os.Getpid()))
}

// paneCtlEnv returns the control-channel env vars for a pane, nil when the
// channel is disabled.
func paneCtlEnv(id int) []string {
	if ctlSocketPath == "" {
		return nil
	}
	return []string{
		envCtlSock + "=" + ctlSocketPath,
		envCtlPane + "=" + strconv.Itoa(id),
	}
}

// parseCtlLine validates and splits one control command (without the pane id
// prefix): "focus", "notify <text>", "send <target> <text>",
// "broadcast <text>", "type <target> <command>", "watch <channel> [--type]",
// "unwatch <channel>" or "emit <channel> [text]". broadcast is sugar for
// send with target "all". Channel names are single tokens (no spaces).
func parseCtlLine(line string) (kind, target, text string, typed bool, err error) {
	line = strings.TrimSpace(line)
	switch {
	case line == "focus":
		return "focus", "", "", false, nil
	case line == "notify":
		return "notify", "", "", false, nil
	case strings.HasPrefix(line, "notify "):
		return "notify", "", strings.TrimSpace(strings.TrimPrefix(line, "notify")), false, nil
	case strings.HasPrefix(line, "send "):
		t, rest, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "send")), " ")
		if t == "" {
			break
		}
		return "send", t, strings.TrimSpace(rest), false, nil
	case strings.HasPrefix(line, "broadcast "):
		return "send", "all", strings.TrimSpace(strings.TrimPrefix(line, "broadcast")), false, nil
	case line == "broadcast":
		return "send", "all", "", false, nil
	case strings.HasPrefix(line, "type "):
		t, rest, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "type")), " ")
		if t == "" || strings.TrimSpace(rest) == "" {
			break
		}
		return "type", t, strings.TrimSpace(rest), false, nil
	case strings.HasPrefix(line, "watch "):
		ch, rest, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "watch")), " ")
		rest = strings.TrimSpace(rest)
		if ch == "" || (rest != "" && rest != "--type") {
			break
		}
		return "watch", ch, "", rest == "--type", nil
	case strings.HasPrefix(line, "unwatch "):
		ch := strings.TrimSpace(strings.TrimPrefix(line, "unwatch"))
		if ch == "" || strings.Contains(ch, " ") {
			break
		}
		return "unwatch", ch, "", false, nil
	case strings.HasPrefix(line, "emit "):
		ch, rest, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "emit")), " ")
		if ch == "" {
			break
		}
		return "emit", ch, strings.TrimSpace(rest), false, nil
	}
	return "", "", "", false, fmt.Errorf("unknown command %q (want: notify [text] | focus | send <target> [text] | broadcast [text] | type <target> <command> | watch <channel> [--type] | unwatch <channel> | emit <channel> [text])", line)
}

// startCtlServer listens on a unix socket and forwards parsed commands to
// ch. Accept/parse errors are dropped silently; closing the listener stops
// the loop.
func startCtlServer(path string, ch chan<- ctlMsg) (net.Listener, error) {
	_ = os.Remove(path) // stale socket from a crashed instance
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveCtlConn(c, ch)
		}
	}()
	return ln, nil
}

func serveCtlConn(c net.Conn, ch chan<- ctlMsg) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		idStr, rest, _ := strings.Cut(sc.Text(), " ")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		kind, target, text, typed, err := parseCtlLine(rest)
		if err != nil {
			continue
		}
		ch <- ctlMsg{paneID: id, kind: kind, target: target, text: text, typed: typed}
	}
}

// runCtlClient implements `pano ctl …`: validate the command, then send one
// line to the instance socket named in the environment.
func runCtlClient(args []string) int {
	sock, pane := os.Getenv(envCtlSock), os.Getenv(envCtlPane)
	if sock == "" || pane == "" {
		fmt.Fprintln(os.Stderr, "pano ctl: not inside a pano pane ("+envCtlSock+"/"+envCtlPane+" unset)")
		return 1
	}
	line := strings.Join(args, " ")
	if _, _, _, _, err := parseCtlLine(line); err != nil {
		fmt.Fprintln(os.Stderr, "pano ctl:", err)
		return 2
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pano ctl:", err)
		return 1
	}
	defer c.Close()
	fmt.Fprintf(c, "%s %s\n", pane, line)
	return 0
}
