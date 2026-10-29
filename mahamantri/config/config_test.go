package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeYAML = `
linear:
  signingSecret: s
  listenAddr: ":8080"
  botUserID: bot
  botHandle: senapati
bridge:
  trustedUserIDs: [u-owner]
`

const opencodeYAML = `
mode: opencode
opencode:
  serverURL: http://127.0.0.1:4096
  password: pw
  model: {providerID: acme, id: big}
linear:
  signingSecret: s
  listenAddr: ":8080"
  botUserID: bot
  botHandle: senapati
`

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mahamantri.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeIsTheDefaultModeAndNeedsNoOpencode(t *testing.T) {
	path := writeConfig(t, claudeYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if c.Mode != "claude" || c.Bridge.ListenAddr != "127.0.0.1:4098" || c.Bridge.Store != filepath.Join(dir, "mahamantri.db") {
		t.Errorf("defaults: mode %q bridge %+v", c.Mode, c.Bridge)
	}
	if !strings.HasSuffix(c.Bridge.DiscoveryFile, filepath.Join("samrajya", "bridge.json")) || !filepath.IsAbs(c.Bridge.DiscoveryFile) {
		t.Errorf("discovery file = %q", c.Bridge.DiscoveryFile)
	}
	if c.Bridge.MainDirectory != "" || len(c.Bridge.TrustedUserIDs) != 1 || c.Linear.BotName != "Senapati" {
		t.Errorf("config = %+v", c)
	}
}

func TestOpencodeModeNeedsItsServer(t *testing.T) {
	c, err := Load(writeConfig(t, opencodeYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(c.Opencode.InstructionsFile, filepath.Join("senapati", "senapati.md")) {
		t.Errorf("instructions = %q", c.Opencode.InstructionsFile)
	}
	t.Setenv("OPENCODE_SERVER_PASSWORD", "")
	for name, yaml := range map[string]string{
		"no server":   strings.Replace(opencodeYAML, "serverURL: http://127.0.0.1:4096", "serverURL: \"\"", 1),
		"bad server":  strings.Replace(opencodeYAML, "http://127.0.0.1:4096", "localhost:4096", 1),
		"no password": strings.Replace(opencodeYAML, "password: pw", "password: \"\"", 1),
		"no provider": strings.Replace(opencodeYAML, "providerID: acme, ", "", 1),
	} {
		if _, err := Load(writeConfig(t, yaml)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestEnvFallbacksFillEmptySecrets(t *testing.T) {
	t.Setenv("LINEAR_SIGNING_SECRET", "from-env")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "pw-env")
	yaml := strings.Replace(strings.Replace(opencodeYAML, "signingSecret: s", "signingSecret: \"\"", 1), "password: pw", "password: \"\"", 1)
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if c.Linear.SigningSecret != "from-env" || c.Opencode.Password != "pw-env" {
		t.Errorf("secrets = %q %q", c.Linear.SigningSecret, c.Opencode.Password)
	}
}

func TestValidation(t *testing.T) {
	t.Setenv("LINEAR_SIGNING_SECRET", "")
	for name, yaml := range map[string]string{
		"unknown key":       claudeYAML + "harness: claude\n",
		"unknown mode":      "mode: managed\n" + claudeYAML,
		"no secret":         strings.Replace(claudeYAML, "signingSecret: s", "signingSecret: \"\"", 1),
		"no listen":         strings.Replace(claudeYAML, "listenAddr: \":8080\"", "listenAddr: \"\"", 1),
		"no bot id":         strings.Replace(claudeYAML, "botUserID: bot", "botUserID: \"\"", 1),
		"no handle":         strings.Replace(claudeYAML, "botHandle: senapati", "botHandle: \"\"", 1),
		"public bridge":     claudeYAML + "  listenAddr: \":4098\"\n",
		"missing main dir":  claudeYAML + "  mainDirectory: /nonexistent/dir\n",
		"missing workspace": "workspace: /nonexistent/ws\n" + claudeYAML,
	} {
		if _, err := Load(writeConfig(t, yaml)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestPathsResolveAgainstTheConfigAndMainDirectoryDefaultsToTheWorkspace(t *testing.T) {
	path := writeConfig(t, "workspace: ws\nbrain: mem\n"+claudeYAML)
	dir := filepath.Dir(path)
	os.Mkdir(filepath.Join(dir, "ws"), 0o755)
	t.Chdir(t.TempDir())
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != filepath.Join(dir, "ws") || c.Bridge.MainDirectory != c.Workspace || c.Brain != filepath.Join(dir, "mem") {
		t.Errorf("paths: workspace %q main %q brain %q", c.Workspace, c.Bridge.MainDirectory, c.Brain)
	}
}
