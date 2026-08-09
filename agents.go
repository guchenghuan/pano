package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// agentRule is one compiled screen-match rule: when the pattern hits the
// pane's trailing screen lines, the pane is considered at that level.
type agentRule struct {
	re    *regexp.Regexp
	level attentionLevel
}

// agentConfig holds the agent process-name set and the screen-match rules,
// pre-split into priority tiers: blocked (waiting for confirmation) rules
// are always checked before working rules, so a stale "working" line can
// never mask a fresh confirmation prompt.
type agentConfig struct {
	names   map[string]bool
	blocked []agentRule
	working []agentRule
}

// agentCfg is the live configuration: the built-in defaults unless main
// loaded a user config file. Tests may replace it (restore via t.Cleanup).
var agentCfg = defaultAgentConfig()

// defaultAgentConfig mirrors the historical hardcoded table; zero-config
// behaviour is unchanged.
func defaultAgentConfig() agentConfig {
	return agentConfig{
		names: map[string]bool{
			"claude":   true,
			"codex":    true,
			"opencode": true,
			"gemini":   true,
			"kimi":     true,
		},
		blocked: []agentRule{
			{regexp.MustCompile(`(?i)do you want to proceed`), attnUnread},
			{regexp.MustCompile(`(?i)❯\s*\d+\.\s*yes`), attnUnread},
			{regexp.MustCompile(`(?i)\(y/n\)|allow\?|approve`), attnUnread},
		},
		working: []agentRule{
			{regexp.MustCompile(`(?i)esc to interrupt`), attnActive},
		},
	}
}

// agentsFile is the on-disk schema of ~/.config/pano/agents.toml.
type agentsFile struct {
	Names []string `toml:"names"`
	Rules []struct {
		Pattern string `toml:"pattern"`
		Level   string `toml:"level"`
	} `toml:"rules"`
}

// configDir is the per-user config directory: $XDG_CONFIG_HOME/pano, else
// ~/.config/pano ("" when the home directory is unknown).
func configDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pano")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "pano")
	}
	return ""
}

// agentConfigPath is the user config file location ("" when unknown).
func agentConfigPath() string {
	if d := configDir(); d != "" {
		return filepath.Join(d, "agents.toml")
	}
	return ""
}

// loadAgentConfig reads and compiles the config at path. A missing file is
// not an error: the defaults are returned. A malformed file falls back to
// the defaults with a non-nil error. Individually broken rules (bad regex,
// unknown level) are skipped and reported as warnings. Sections left out of
// the file (names / rules) keep their defaults.
func loadAgentConfig(path string) (cfg agentConfig, warnings []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultAgentConfig(), nil, nil
		}
		return defaultAgentConfig(), nil, err
	}
	var f agentsFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return defaultAgentConfig(), nil, fmt.Errorf("parse %s: %w", path, err)
	}

	def := defaultAgentConfig()
	cfg.names = def.names
	if f.Names != nil {
		cfg.names = map[string]bool{}
		for _, n := range f.Names {
			cfg.names[n] = true
		}
	}
	if f.Rules == nil {
		cfg.blocked, cfg.working = def.blocked, def.working
		return cfg, warnings, nil
	}
	levels := map[string]attentionLevel{"blocked": attnUnread, "working": attnActive}
	for i, r := range f.Rules {
		lvl, ok := levels[r.Level]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("rule %d: unknown level %q (want blocked|working)", i+1, r.Level))
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("rule %d: bad pattern: %v", i+1, err))
			continue
		}
		rule := agentRule{re: re, level: lvl}
		if lvl == attnUnread {
			cfg.blocked = append(cfg.blocked, rule)
		} else {
			cfg.working = append(cfg.working, rule)
		}
	}
	return cfg, warnings, nil
}
