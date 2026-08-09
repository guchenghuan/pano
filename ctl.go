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
//
// The socket is per-instance (pid-named, mode 0600), removed on exit.
// Commands are single text lines "<pane-id> <command>".

// ctlMsg is one command received over the control socket.
type ctlMsg struct {
	paneID int
	kind   string // "notify" or "focus"
	text   string
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
// prefix): "focus" or "notify <text>".
func parseCtlLine(line string) (kind, text string, err error) {
	line = strings.TrimSpace(line)
	switch {
	case line == "focus":
		return "focus", "", nil
	case line == "notify":
		return "notify", "", nil
	case strings.HasPrefix(line, "notify "):
		return "notify", strings.TrimSpace(strings.TrimPrefix(line, "notify")), nil
	}
	return "", "", fmt.Errorf("unknown command %q (want: notify [text] | focus)", line)
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
		kind, text, err := parseCtlLine(rest)
		if err != nil {
			continue
		}
		ch <- ctlMsg{paneID: id, kind: kind, text: text}
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
	if _, _, err := parseCtlLine(line); err != nil {
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
