package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	// Mode is what the main session runs on. Either way it is one hands-on
	// assistant session that does the work, and mahamantri is its bridge:
	// the durable inbox of Linear pings, the tasks they become, the journal
	// of external actions and the brain.
	//
	//   claude   - an interactive Claude Code session you start (reachable
	//              through Remote Control) with the Samrajya channel plugin,
	//              which pulls pings from the bridge. The default.
	//   opencode - a session on an opencode server; mahamantri creates it
	//              and prompts it with each ping. It reaches the bridge
	//              through the same plugin's MCP server.
	Mode string `yaml:"mode"`
	// Workspace is the directory holding the projects this system works on,
	// one directory per project (e.g. ../repos). Empty: none.
	Workspace string `yaml:"workspace"`
	// Brain is the directory holding Samrajya's memory of the work (see
	// package brain), e.g. ../brain. Empty: no memory.
	Brain  string `yaml:"brain"`
	Linear struct {
		SigningSecret string `yaml:"signingSecret"`
		ListenAddr    string `yaml:"listenAddr"`
		BotUserID     string `yaml:"botUserID"`
		BotName       string `yaml:"botName"`
		BotHandle     string `yaml:"botHandle"`
		// APIKey is a Linear API key of Senapati's own account (default:
		// $LINEAR_API_KEY). With it, mahamantri reacts to each ping as it
		// arrives, shows on Senapati's Linear status when it is offline or
		// blocked, and tells the person when a ping waits too long.
		// Optional.
		APIKey string `yaml:"apiKey"`
		// APIURL replaces Linear's GraphQL endpoint (for testing).
		APIURL string `yaml:"apiURL"`
		// ReceivedEmoji is the reaction a ping gets once stored. Default 👀.
		ReceivedEmoji string `yaml:"receivedEmoji"`
		// NudgeAfter is how long a ping may wait unaccepted before its author
		// is told it is queued. Default 15m; 0s turns nudges off.
		Nudge      string        `yaml:"nudgeAfter"`
		NudgeAfter time.Duration `yaml:"-"`
	} `yaml:"linear"`
	Bridge struct {
		// ListenAddr is the bridge API the main session's tools talk to.
		// Loopback only. Default 127.0.0.1:4098.
		ListenAddr string `yaml:"listenAddr"`
		// Store is the bridge's database. Default mahamantri.db next to the
		// config.
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
		// KeepMain is a command mahamantri runs whenever no main session has
		// been attached for a while, to start one: Claude Code ends a
		// background session after hours idle, and nothing else would bring
		// it back. Mode claude only; empty: nobody restarts it. A relative
		// program path is taken from the config's directory, e.g.
		// ["../integrations/claude-channel/restart-main.sh", "--ensure"].
		KeepMain []string `yaml:"keepMain"`
	} `yaml:"bridge"`
	// Opencode configures mode: opencode.
	Opencode struct {
		ServerURL string `yaml:"serverURL"`
		Password  string `yaml:"password"`
		Agent     string `yaml:"agent"`
		Model     struct {
			ProviderID string `yaml:"providerID"`
			ID         string `yaml:"id"`
		} `yaml:"model"`
		// InstructionsFile is the main session's role, installed as its
		// instructions. Default ../senapati/senapati.md, relative to the
		// config.
		InstructionsFile string `yaml:"instructionsFile"`
	} `yaml:"opencode"`

	// Dir is the directory holding the config file; relative paths in it,
	// and the log file, resolve against it - not against wherever the
	// program happened to be launched from.
	Dir string `yaml:"-"`

	// Agents runs several Claude Code sessions at once (mode claude only):
	// a main that talks to the person and a worker per task, launched and
	// supervised by mahamantri. See docs/design/agents.md.
	Agents struct {
		Enabled    bool `yaml:"enabled"`
		MaxWorkers int  `yaml:"maxWorkers"`
		// NamePrefix starts every session name mahamantri launches; sessions
		// with the prefix that are no agent's current launch are stopped.
		NamePrefix string `yaml:"namePrefix"`
		// Command is the claude executable.
		Command string `yaml:"command"`
		// MainRole and WorkerRole are the role files appended to each
		// session's system prompt.
		MainRole   string `yaml:"mainRole"`
		WorkerRole string `yaml:"workerRole"`
		// MainArgs and WorkerArgs are added to each launch, e.g. --model.
		MainArgs   []string `yaml:"mainArgs"`
		WorkerArgs []string `yaml:"workerArgs"`
		// RemoteControl names the main session's Remote Control session;
		// workers get theirs named after their task. Empty: none.
		RemoteControl string `yaml:"remoteControl"`
		WorkerRemote  bool   `yaml:"workerRemoteControl"`
		// StateDir holds each agent's secret file (default next to the
		// discovery file).
		StateDir string `yaml:"stateDir"`
		// Channel is the channel plugin sessions load.
		Channel string `yaml:"channel"`
	} `yaml:"agents"`
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
	if c.Opencode.Password == "" {
		c.Opencode.Password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	}
	if c.Linear.SigningSecret == "" {
		c.Linear.SigningSecret = os.Getenv("LINEAR_SIGNING_SECRET")
	}
	if c.Linear.APIKey == "" {
		c.Linear.APIKey = os.Getenv("LINEAR_API_KEY")
	}
	if c.Linear.ReceivedEmoji == "" {
		c.Linear.ReceivedEmoji = "👀"
	}
	c.Linear.NudgeAfter = 15 * time.Minute
	if c.Linear.Nudge != "" {
		if c.Linear.NudgeAfter, err = time.ParseDuration(c.Linear.Nudge); err != nil || c.Linear.NudgeAfter < 0 {
			return Config{}, fmt.Errorf("config %s: linear.nudgeAfter %q is not a duration like 15m", abs, c.Linear.Nudge)
		}
	}
	if c.Linear.BotUserID != "" && c.Linear.BotName == "" {
		c.Linear.BotName = "Senapati"
	}
	if c.Mode == "" {
		c.Mode = "claude"
	}
	if c.Bridge.ListenAddr == "" {
		c.Bridge.ListenAddr = "127.0.0.1:4098"
	}
	if c.Bridge.Store == "" {
		c.Bridge.Store = "mahamantri.db"
	}
	if c.Bridge.DiscoveryFile == "" {
		c.Bridge.DiscoveryFile = DefaultDiscoveryFile()
	}
	if c.Bridge.MainDirectory == "" {
		c.Bridge.MainDirectory = c.Workspace
	}
	if c.Opencode.InstructionsFile == "" {
		c.Opencode.InstructionsFile = "../senapati/senapati.md"
	}
	if c.Agents.MaxWorkers == 0 {
		c.Agents.MaxWorkers = 2
	}
	if c.Agents.NamePrefix == "" {
		c.Agents.NamePrefix = "samrajya"
	}
	if c.Agents.Command == "" {
		c.Agents.Command = "claude"
	}
	if c.Agents.MainRole == "" {
		c.Agents.MainRole = "../senapati/main.md"
	}
	if c.Agents.WorkerRole == "" {
		c.Agents.WorkerRole = "../senapati/worker.md"
	}
	if c.Agents.StateDir == "" {
		c.Agents.StateDir = filepath.Join(filepath.Dir(c.Bridge.DiscoveryFile), "agents")
	}
	if c.Agents.Channel == "" {
		c.Agents.Channel = "plugin:samrajya@samrajya"
	}
	paths := []*string{&c.Workspace, &c.Brain, &c.Bridge.Store, &c.Bridge.DiscoveryFile, &c.Bridge.MainDirectory, &c.Opencode.InstructionsFile,
		&c.Agents.MainRole, &c.Agents.WorkerRole, &c.Agents.StateDir}
	if len(c.Bridge.KeepMain) > 0 && strings.ContainsRune(c.Bridge.KeepMain[0], '/') {
		paths = append(paths, &c.Bridge.KeepMain[0])
	}
	for _, p := range paths {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.Dir, *p)
		}
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", abs, err)
	}
	return c, nil
}

// DefaultDiscoveryFile is where mahamantri tells the plugin how to reach the
// bridge, unless configured otherwise.
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
	case "claude":
	case "opencode":
		u, err := url.Parse(c.Opencode.ServerURL)
		switch {
		case c.Opencode.ServerURL == "":
			return fmt.Errorf("opencode.serverURL is required in mode opencode")
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			return fmt.Errorf("opencode.serverURL %q must look like http://localhost:4096", c.Opencode.ServerURL)
		case c.Opencode.Password == "":
			return fmt.Errorf("opencode.password is required (or set OPENCODE_SERVER_PASSWORD)")
		case c.Opencode.Model.ID != "" && c.Opencode.Model.ProviderID == "":
			return fmt.Errorf("opencode.model needs providerID as well as id")
		}
	default:
		return fmt.Errorf("mode %q must be \"claude\" or \"opencode\"", c.Mode)
	}
	switch {
	case c.Linear.SigningSecret == "":
		return fmt.Errorf("linear.signingSecret is required (or set LINEAR_SIGNING_SECRET)")
	case c.Linear.ListenAddr == "":
		return fmt.Errorf("linear.listenAddr is required")
	case c.Linear.BotUserID == "":
		return fmt.Errorf("linear.botUserID is required: Senapati's own Linear user id. Without it its own comments " +
			"come back to it as pings and replies to it can't be recognised. It is actor.id on any comment Senapati made")
	case c.Linear.BotHandle == "":
		return fmt.Errorf("linear.botHandle is required: what comments @-mention Senapati as (the part after the @), " +
			"or no mention would ever reach it")
	case !loopback(c.Bridge.ListenAddr):
		return fmt.Errorf("bridge.listenAddr %q must be a loopback address such as 127.0.0.1:4098", c.Bridge.ListenAddr)
	}
	if c.Agents.Enabled {
		switch {
		case c.Mode != "claude":
			return fmt.Errorf("agents.enabled needs mode claude")
		case c.Bridge.MainDirectory == "":
			return fmt.Errorf("agents.enabled needs bridge.mainDirectory (or workspace): where sessions run")
		case len(c.Bridge.KeepMain) > 0:
			return fmt.Errorf("agents.enabled replaces bridge.keepMain: remove keepMain, the supervisor keeps the main running")
		case c.Agents.MaxWorkers < 1:
			return fmt.Errorf("agents.maxWorkers must be at least 1")
		case !validPrefix(c.Agents.NamePrefix):
			return fmt.Errorf("agents.namePrefix %q must be letters, digits and dashes", c.Agents.NamePrefix)
		}
		for name, f := range map[string]string{"agents.mainRole": c.Agents.MainRole, "agents.workerRole": c.Agents.WorkerRole} {
			if st, err := os.Stat(f); err != nil || st.IsDir() {
				return fmt.Errorf("%s %q is not a file", name, f)
			}
		}
	}
	for name, dir := range map[string]string{"workspace": c.Workspace, "bridge.mainDirectory": c.Bridge.MainDirectory} {
		if dir == "" {
			continue
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return fmt.Errorf("%s %q is not a directory", name, dir)
		}
	}
	return nil
}

func validPrefix(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// loopback reports whether addr binds only the local machine. An empty host
// (":4098") binds every interface and does not count.
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
