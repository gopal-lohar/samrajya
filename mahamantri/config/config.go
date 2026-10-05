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
	State  struct {
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
	for _, p := range []*string{&c.Attention.RegistryFile, &c.State.SenapatiFile, &c.State.LinearThreadsFile, &c.Senapati.InstructionsFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(c.Dir, *p)
		}
	}
}

func (c Config) Validate() error {
	u, err := url.Parse(c.Opencode.ServerURL)
	switch {
	case c.Opencode.ServerURL == "":
		return fmt.Errorf("opencode.serverURL is required")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return fmt.Errorf("opencode.serverURL %q must look like http://localhost:4096", c.Opencode.ServerURL)
	case c.Opencode.Password == "":
		return fmt.Errorf("opencode.password is required (or set OPENCODE_SERVER_PASSWORD)")
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
	case c.Attention.ListenAddr == "":
		return fmt.Errorf("attention.listenAddr is required")
	case !loopback(c.Attention.ListenAddr):
		return fmt.Errorf("attention.listenAddr %q must be a loopback address such as 127.0.0.1:4097: "+
			"it serves the opencode API with mahamantri's credentials, unauthenticated", c.Attention.ListenAddr)
	case c.Attention.RegistryFile == "":
		return fmt.Errorf("attention.registryFile is required")
	case c.State.SenapatiFile == "":
		return fmt.Errorf("state.senapatiFile is required")
	case c.Senapati.RotationThreshold <= 0 || c.Senapati.RotationThreshold > 1:
		return fmt.Errorf("senapati.rotationThreshold must be between 0 and 1")
	}
	if c.Sainik.DefaultModel != "" && !isModelRef(c.Sainik.DefaultModel) {
		return fmt.Errorf("sainik.defaultModel %q must look like provider/id", c.Sainik.DefaultModel)
	}
	for i, m := range c.Models {
		if !isModelRef(m.ID) {
			return fmt.Errorf("models[%d].id %q must look like provider/id", i, m.ID)
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
