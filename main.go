package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ctl" {
		os.Exit(runCtlClient(os.Args[2:]))
	}
	if path := agentConfigPath(); path != "" {
		cfg, warnings, err := loadAgentConfig(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pano:", err, "(using built-in defaults)")
		} else {
			agentCfg = cfg
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "pano: agents config:", w)
			}
		}
	}
	m := newModel()
	// External control channel: per-instance unix socket; the path and pane
	// id reach shells via PANO_SOCK/PANO_PANE (see ctl.go).
	ctlCh := make(chan ctlMsg, 16)
	if ln, err := startCtlServer(defaultCtlSocketPath(), ctlCh); err != nil {
		fmt.Fprintln(os.Stderr, "pano: ctl socket:", err, "(channel disabled)")
	} else {
		ctlSocketPath = ln.Addr().String()
		defer func() {
			_ = ln.Close()
			_ = os.Remove(ctlSocketPath)
		}()
		m.ctlCh = ctlCh
	}
	if path := sessionPath(); path != "" {
		sf, err := loadSession(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pano:", err, "(starting fresh)")
		} else if sf != nil {
			m.pendingRestore = sf
		}
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "pano:", err)
		os.Exit(1)
	}
}
