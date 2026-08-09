package main

import "github.com/charmbracelet/lipgloss"

// Theme: "轻奢淡雅" — muted, matte, breathing room. All colors are defined
// here as named constants so the look can be re-tuned in one place (this is
// a palette, not a theme system).
var (
	// accent is the signature matte champagne gold: focused frames, mode
	// indicators, agent names, the NAV marker.
	accentColor = lipgloss.Color("#C9B896")
	// accentSoft is a dimmer gold for key labels and secondary accents.
	accentSoftColor = lipgloss.Color("#A89878")

	// Text hierarchy: three descending brightness levels.
	textPrimary   = lipgloss.Color("#D3D7E0") // warm soft white: focused titles, button actions
	textSecondary = lipgloss.Color("#7A7F9A") // mid grey: unfocused titles, status text
	textTertiary  = lipgloss.Color("#5A5E78") // faintest: branch/proc, hints

	// frameBlur is the unfocused frame: quiet but clearly discernible (it
	// is the only thing delineating sidebar minis and grid panes).
	frameBlur = lipgloss.Color("#3D4256")

	// cursorColor is the hardware cursor block: plain white, always visible
	// against any shell color scheme.
	cursorColor = lipgloss.Color("#FFFFFF")

	// Attention dots (all desaturated, matte).
	dotActiveColor = lipgloss.Color("#A3BE8C") // sage: recent output / agent working
	dotUnreadColor = lipgloss.Color("#D8B26E") // muted apricot: unread / agent blocked
	dotNotifyColor = lipgloss.Color("#C98A96") // dry rose: BEL / OSC notify, also QUIT?/CLOSE?
	dotIdleColor   = lipgloss.Color("#62667A") // grey: idle

	// quitColor is the dry-rose used for all warning states (text/thin
	// frame, never a solid block).
	quitColor = dotNotifyColor

	focusFrameStyle = lipgloss.NewStyle().Foreground(accentColor)
	blurFrameStyle  = lipgloss.NewStyle().Foreground(frameBlur)
	focusTitleStyle = lipgloss.NewStyle().Foreground(textPrimary)
	blurTitleStyle  = lipgloss.NewStyle().Foreground(textSecondary)

	// close-select mode: every pane becomes visible, the focused one keeps
	// the accent as the default target.
	selectFrameStyle = lipgloss.NewStyle().Foreground(textTertiary)
	selectTitleStyle = lipgloss.NewStyle().Foreground(textSecondary)

	// Badges are text-only now (no solid blocks): quiet but legible.
	prefixBadgeStyle = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	navBadgeStyle    = lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	editBadgeStyle   = lipgloss.NewStyle().Foreground(textSecondary)
	scrollBadgeStyle = lipgloss.NewStyle().Foreground(dotUnreadColor)
	quitBadgeStyle   = lipgloss.NewStyle().Foreground(quitColor).Bold(true)
	quitTextStyle    = lipgloss.NewStyle().Foreground(quitColor)
	quitFrameStyle   = lipgloss.NewStyle().Foreground(quitColor)
	quitTitleStyle   = lipgloss.NewStyle().Foreground(quitColor)
	closeBadgeStyle  = quitBadgeStyle // close-select shares the quit warning color

	attnUnreadFrameStyle = lipgloss.NewStyle().Foreground(dotUnreadColor)
	agentNameStyle       = lipgloss.NewStyle().Foreground(accentColor)

	statusBarStyle = lipgloss.NewStyle().Foreground(textSecondary)
	statusKeyStyle = lipgloss.NewStyle().Foreground(textTertiary)

	// Ghost buttons: no fill — dim gold key label, soft-white action.
	btnKeyStyle = lipgloss.NewStyle().Foreground(accentSoftColor)
	btnActStyle = lipgloss.NewStyle().Foreground(textPrimary)
)
