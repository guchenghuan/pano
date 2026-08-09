package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// keyBytes translates a bubbletea key message into the raw bytes a terminal
// application would receive. appCursor selects the application-cursor
// sequences for the arrow keys (DECCKM mode of the focused pane).
func keyBytes(msg tea.KeyMsg, appCursor bool) []byte {
	k := tea.Key(msg)

	// C0 control bytes (0x00-0x1f) map 1:1: ctrl+a..ctrl+z, enter (\r),
	// tab (\t), esc (0x1b). Backspace (DEL, 0x7f) likewise.
	if k.Type >= tea.KeyNull && k.Type <= tea.KeyCtrlUnderscore {
		return []byte{byte(k.Type)}
	}
	if k.Type == tea.KeyBackspace {
		return []byte{0x7f}
	}

	switch k.Type {
	case tea.KeyRunes:
		b := []byte(string(k.Runes))
		if k.Alt {
			return append([]byte{0x1b}, b...)
		}
		return b
	case tea.KeySpace:
		return []byte{' '}
	case tea.KeyUp:
		if appCursor {
			return []byte("\x1bOA")
		}
		return []byte("\x1b[A")
	case tea.KeyDown:
		if appCursor {
			return []byte("\x1bOB")
		}
		return []byte("\x1b[B")
	case tea.KeyRight:
		if appCursor {
			return []byte("\x1bOC")
		}
		return []byte("\x1b[C")
	case tea.KeyLeft:
		if appCursor {
			return []byte("\x1bOD")
		}
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyInsert:
		return []byte("\x1b[2~")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyF1:
		return []byte("\x1bOP")
	case tea.KeyF2:
		return []byte("\x1bOQ")
	case tea.KeyF3:
		return []byte("\x1bOR")
	case tea.KeyF4:
		return []byte("\x1bOS")
	case tea.KeyF5:
		return []byte("\x1b[15~")
	case tea.KeyF6:
		return []byte("\x1b[17~")
	case tea.KeyF7:
		return []byte("\x1b[18~")
	case tea.KeyF8:
		return []byte("\x1b[19~")
	case tea.KeyF9:
		return []byte("\x1b[20~")
	case tea.KeyF10:
		return []byte("\x1b[21~")
	case tea.KeyF11:
		return []byte("\x1b[23~")
	case tea.KeyF12:
		return []byte("\x1b[24~")
	}
	return nil
}
