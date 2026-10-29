package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

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
	for _, p := range []*string{&c.Workspace, &c.Brain, &c.Bridge.Store, &c.Bridge.DiscoveryFile, &c.Bridge.MainDirectory, &c.Opencode.InstructionsFile} {
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
