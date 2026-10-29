package config

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
	"gopkg.in/yaml.v3"
)

// ModelPolicy is one model Senapati may choose for a session it spawns. Use
// and Credits are free-form guidance written for Senapati (what the model is
// good for, how scarce its credits are) and are rendered into its briefing.
type ModelPolicy struct {
	ID      string `yaml:"id"` // provider/id
	Credits string `yaml:"credits"`
	Use     string `yaml:"use"`
}

type Config struct {
	// Mode is how Senapati runs: "managed" (default) - mahamantri runs it
	// and its sainiks on the harness below, with the TUI - or "native": an
	// interactive Claude Code session you start yourself (reachable through
	// Remote Control) does the work, and mahamantri only keeps the durable
	// inbox, tasks and action journal and feeds Linear pings to it through
	// the Samrajya channel plugin. See Native.
	Mode string `yaml:"mode"`
	// Native configures native mode.
	Native struct {
		// ListenAddr is the bridge API the channel plugin talks to.
		// Loopback only. Default 127.0.0.1:4098.
		ListenAddr string `yaml:"listenAddr"`
		// Store is the bridge's database. Default mahamantri-native.db next
		// to the config.
		Store string `yaml:"store"`
		// DiscoveryFile is where mahamantri writes the bridge's address and
		// a fresh token on every start, for the plugin to read. Default
		// $XDG_STATE_HOME/samrajya/bridge.json (~/.local/state/...).
		DiscoveryFile string `yaml:"discoveryFile"`
		// MainDirectory is where the main session runs. Only a session in
		// this directory attaches as the main one by itself; a session
		// elsewhere that has the plugin loaded stays passive. Default: the
		// workspace.
		MainDirectory string `yaml:"mainDirectory"`
		// TrustedUserIDs are the Linear users whose comments may ask for
		// work. Others' pings still arrive, marked untrusted: information
		// to check with a trusted person, not instructions. Empty: everyone
		// is trusted.
		TrustedUserIDs []string `yaml:"trustedUserIDs"`
	} `yaml:"native"`
	// Harness is what sessions run on: "opencode" (an opencode server) or
	// "claude" (Claude Code, run by mahamantri). Empty means opencode.
	Harness string `yaml:"harness"`
	// Workspace is the directory holding the projects this system works on,
	// one directory per project (e.g. ../repos). Sainiks are told to work
	// only inside it, and cannot be started outside it. Empty: no limit.
	Workspace string `yaml:"workspace"`
	// Brain is the directory holding Samrajya's memory of the work (see
	// package brain), e.g. ../brain. Empty: no memory.
	Brain    string `yaml:"brain"`
	Opencode struct {
		ServerURL string `yaml:"serverURL"`
		Password  string `yaml:"password"`
	} `yaml:"opencode"`
	Linear struct {
		SigningSecret string `yaml:"signingSecret"`
		ListenAddr    string `yaml:"listenAddr"`
		BotUserID     string `yaml:"botUserID"`
		BotName       string `yaml:"botName"`
		BotHandle     string `yaml:"botHandle"`
	} `yaml:"linear"`
	// Attention is mahamantri's local API: the opencode gateway (with
	// credentials) and the attention API. Loopback only.
	Attention struct {
		ListenAddr   string `yaml:"listenAddr"`
		RegistryFile string `yaml:"registryFile"`
	} `yaml:"attention"`
	Sainik struct {
		Directory    string `yaml:"directory"`
		DefaultModel string `yaml:"defaultModel"` // provider/id, used when Senapati names none
		Agent        string `yaml:"agent"`
	} `yaml:"sainik"`
	Models []ModelPolicy `yaml:"models"`
	// Claude configures the Claude Code harness (harness: claude).
	Claude struct {
		Binary string `yaml:"binary"` // default "claude"
		// StateFile remembers the Claude Code sessions mahamantri runs.
		// Defaults to mahamantri-claude.json next to the config.
		StateFile string `yaml:"stateFile"`
		// IdleExit is how long a session's process stays up with nothing to
		// do before it exits; the next message resumes it. Default 10m.
		IdleExit time.Duration `yaml:"idleExit"`
		// UsageWarnAt is the share of a subscription window (0-1) at which
		// the TUI warns. Default 0.9.
		UsageWarnAt float64 `yaml:"usageWarnAt"`
		// SenapatiAllowedTools / SenapatiDeniedTools add to Senapati's tool
		// lockdown (Claude Code tool rules, e.g. "mcp__github" or
		// "mcp__linear__delete_comment"); a denial wins over any allow.
		SenapatiAllowedTools []string `yaml:"senapatiAllowedTools"`
		SenapatiDeniedTools  []string `yaml:"senapatiDeniedTools"`
		// SainikArgs are extra claude flags for every sainik.
		SainikArgs []string `yaml:"sainikArgs"`
		// ContextWindows gives the context window (tokens) of models Claude
		// Code does not know: it assumes 200k for any model routed through a
		// proxy, which makes Senapati rotate too early on, e.g., gpt-6-sol.
		ContextWindows map[string]int `yaml:"contextWindows"`
	} `yaml:"claude"`
	State struct {
		SenapatiFile string `yaml:"senapatiFile"`
		// LinearThreadsFile remembers the Linear comment threads Senapati
		// has written in, so replies in them reach it. Defaults to
		// mahamantri-linear-threads.json next to the config.
		LinearThreadsFile string `yaml:"linearThreadsFile"`
	} `yaml:"state"`
	Senapati struct {
		Agent     string `yaml:"agent"`
		Directory string `yaml:"directory"`
		Model     struct {
			ProviderID string `yaml:"providerID"`
			ID         string `yaml:"id"`
		} `yaml:"model"`
		RotationThreshold float64 `yaml:"rotationThreshold"`
		InstructionsFile  string  `yaml:"instructionsFile"`
		// FallbackModel (claude harness): when Senapati hits a usage limit it
		// is switched to this model - on another provider, e.g. a Codex model -
		// and continues at once instead of pausing until the reset.
		FallbackModel string `yaml:"fallbackModel"`
		// ExtraPermissions are appended to the rules that restrict Senapati
		// to Mahamantri and Linear (see senapati.Permissions) - e.g. another
		// MCP server it should be able to use.
		ExtraPermissions []opencode.PermissionRule `yaml:"extraPermissions"`
	} `yaml:"senapati"`

	// Dir is the directory holding the config file; relative paths in it,
	// and the log file, resolve against it - not against wherever the
	// program happened to be launched from, which would silently start a
	// brand-new Senapati and an empty registry.
	Dir string `yaml:"-"`
}

func (c Config) LogFile() string { return filepath.Join(c.Dir, "mahamantri.log") }

// Load reads and parses path, applies env-var fallbacks for secrets, resolves
// relative paths, and validates the result. Unknown keys are an error so a
// typo can't silently fall back to a default.
func Load(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", abs, err)
	}
	c.Dir = filepath.Dir(abs)
	c.applyEnvFallbacks()
	if c.Linear.BotUserID != "" && c.Linear.BotName == "" {
		c.Linear.BotName = "Senapati"
	}
	if c.State.LinearThreadsFile == "" {
		c.State.LinearThreadsFile = "mahamantri-linear-threads.json"
	}
	if c.Mode == "" {
		c.Mode = "managed"
	}
	if c.Native.ListenAddr == "" {
		c.Native.ListenAddr = "127.0.0.1:4098"
	}
	if c.Native.Store == "" {
		c.Native.Store = "mahamantri-native.db"
	}
	if c.Native.DiscoveryFile == "" {
		c.Native.DiscoveryFile = DefaultDiscoveryFile()
	}
	if c.Native.MainDirectory == "" {
		c.Native.MainDirectory = c.Workspace
	}
	if c.Harness == "" {
		c.Harness = "opencode"
	}
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
	}
	if c.Claude.StateFile == "" {
		c.Claude.StateFile = "mahamantri-claude.json"
	}
	if c.Claude.IdleExit == 0 {
		c.Claude.IdleExit = 10 * time.Minute
	}
	if c.Claude.UsageWarnAt == 0 {
		c.Claude.UsageWarnAt = 0.9
	}
	c.resolvePaths()
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", abs, err)
	}
	return c, nil
}

// applyEnvFallbacks lets secrets live in the environment instead of the
// YAML file on disk - a real env var only fills in when the YAML value is
// empty.
func (c *Config) applyEnvFallbacks() {
	if c.Opencode.Password == "" {
		c.Opencode.Password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	}
	if c.Linear.SigningSecret == "" {
		c.Linear.SigningSecret = os.Getenv("LINEAR_SIGNING_SECRET")
	}
}

func (c *Config) resolvePaths() {
	for _, p := range []*string{&c.Attention.RegistryFile, &c.State.SenapatiFile, &c.State.LinearThreadsFile, &c.Senapati.InstructionsFile, &c.Claude.StateFile, &c.Workspace, &c.Sainik.Directory, &c.Brain, &c.Native.Store, &c.Native.DiscoveryFile, &c.Native.MainDirectory} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.Dir, *p)
		}
	}
}

// DefaultDiscoveryFile is where native mode tells the channel plugin how to
// reach it, unless configured otherwise.
func DefaultDiscoveryFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "samrajya", "bridge.json")
}

func (c Config) Validate() error {
	switch c.Mode {
	case "managed":
	case "native":
		return c.validateNative()
	default:
		return fmt.Errorf("mode %q must be \"managed\" or \"native\"", c.Mode)
	}
	switch c.Harness {
	case "opencode", "":
		u, err := url.Parse(c.Opencode.ServerURL)
		switch {
		case c.Opencode.ServerURL == "":
			return fmt.Errorf("opencode.serverURL is required")
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return fmt.Errorf("opencode.serverURL %q must look like http://localhost:4096", c.Opencode.ServerURL)
		case c.Opencode.Password == "":
			return fmt.Errorf("opencode.password is required (or set OPENCODE_SERVER_PASSWORD)")
		}
	case "claude":
		if c.Claude.UsageWarnAt < 0 || c.Claude.UsageWarnAt > 1 {
			return fmt.Errorf("claude.usageWarnAt must be between 0 and 1")
		}
	default:
		return fmt.Errorf("harness %q must be \"opencode\" or \"claude\"", c.Harness)
	}
	if err := c.validateLinear(); err != nil {
		return err
	}
	switch {
	case c.Attention.ListenAddr == "":
		return fmt.Errorf("attention.listenAddr is required")
	case !loopback(c.Attention.ListenAddr):
		return fmt.Errorf("attention.listenAddr %q must be a loopback address such as 127.0.0.1:4097: "+
			"it drives agent sessions with mahamantri's credentials, unauthenticated", c.Attention.ListenAddr)
	case c.Attention.RegistryFile == "":
		return fmt.Errorf("attention.registryFile is required")
	case c.State.SenapatiFile == "":
		return fmt.Errorf("state.senapatiFile is required")
	case c.Senapati.RotationThreshold <= 0 || c.Senapati.RotationThreshold > 1:
		return fmt.Errorf("senapati.rotationThreshold must be between 0 and 1")
	}
	if c.Workspace != "" {
		if st, err := os.Stat(c.Workspace); err != nil || !st.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", c.Workspace)
		}
		if c.Sainik.Directory != "" && !Within(c.Workspace, c.Sainik.Directory) {
			return fmt.Errorf("sainik.directory %q must be inside workspace %q", c.Sainik.Directory, c.Workspace)
		}
	}
	// opencode names a model provider/id; Claude Code by its id alone.
	if c.Harness != "claude" {
		if c.Sainik.DefaultModel != "" && !isModelRef(c.Sainik.DefaultModel) {
			return fmt.Errorf("sainik.defaultModel %q must look like provider/id", c.Sainik.DefaultModel)
		}
		for i, m := range c.Models {
			if !isModelRef(m.ID) {
				return fmt.Errorf("models[%d].id %q must look like provider/id", i, m.ID)
			}
		}
	}
	for i, r := range c.Senapati.ExtraPermissions {
		if r.Action == "" || r.Resource == "" || (r.Effect != "allow" && r.Effect != "deny") {
			return fmt.Errorf("senapati.extraPermissions[%d] needs action, resource and effect (allow or deny) - "+
				"\"ask\" would block Senapati, there is nobody to answer it", i)
		}
	}
	return nil
}

// ProjectOf is the workspace project dir belongs to - the first directory
// below the workspace - or "" if dir is the workspace itself or outside it.

// validateNative checks what native mode uses: Linear, the bridge and the
// brain/workspace paths - not the managed harness, registry or Senapati
// settings, which it ignores.
func (c Config) validateNative() error {
	if err := c.validateLinear(); err != nil {
		return err
	}
	if !loopback(c.Native.ListenAddr) {
		return fmt.Errorf("native.listenAddr %q must be a loopback address such as 127.0.0.1:4098", c.Native.ListenAddr)
	}
	if c.Workspace != "" {
		if st, err := os.Stat(c.Workspace); err != nil || !st.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", c.Workspace)
		}
	}
	if c.Native.MainDirectory != "" {
		if st, err := os.Stat(c.Native.MainDirectory); err != nil || !st.IsDir() {
			return fmt.Errorf("native.mainDirectory %q is not a directory", c.Native.MainDirectory)
		}
	}
	return nil
}

func (c Config) validateLinear() error {
	switch {
	case c.Linear.SigningSecret == "":
		return fmt.Errorf("linear.signingSecret is required (or set LINEAR_SIGNING_SECRET)")
	case c.Linear.ListenAddr == "":
		return fmt.Errorf("linear.listenAddr is required")
	case c.Linear.BotUserID == "":
		return fmt.Errorf("linear.botUserID is required: Senapati's own Linear user id. Without it its own comments " +
			"come back to it as pings and replies to it can't be recognised. It is actor.id on any comment Senapati " +
			"made, in mahamantri.log")
	case c.Linear.BotHandle == "":
		return fmt.Errorf("linear.botHandle is required: what comments @-mention Senapati as (the part after the @), " +
			"or no mention would ever reach it")
	}
	return nil
}

func ProjectOf(workspace, dir string) string {
	if workspace == "" || !Within(workspace, dir) {
		return ""
	}
	rel, _ := filepath.Rel(filepath.Clean(workspace), filepath.Clean(dir))
	if rel == "." {
		return ""
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return first
}

// Within reports whether path is dir or inside it.
func Within(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

func isModelRef(s string) bool {
	provider, id, ok := strings.Cut(s, "/")
	return ok && provider != "" && id != ""
}

// loopback reports whether addr binds only the local machine. An empty host
// (":4097") binds every interface and does not count.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Watch polls path every interval and calls onChange whenever the file's
// contents change, with the freshly loaded config or the reason it failed to
// load (e.g. mid-edit). It doesn't fire for the file's state at call time.
func Watch(ctx context.Context, path string, interval time.Duration, onChange func(Config, error)) {
	last := stamp(path)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if now := stamp(path); now != last {
				last = now
				onChange(Load(path))
			}
		case <-ctx.Done():
			return
		}
	}
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func stamp(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{info.ModTime(), info.Size()}
}
