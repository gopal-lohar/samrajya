package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
opencode:
  serverURL: http://localhost:4096
  password: secret-pw
linear:
  signingSecret: lin_wh_test
  listenAddr: ":45821"
  botUserID: "076c966a-da5b-447e-a890-4b0648465fbb"
  botHandle: "gingermagenta"
attention:
  listenAddr: "127.0.0.1:4097"
  registryFile: "./registry.json"
state:
  senapatiFile: "./senapati.json"
senapati:
  rotationThreshold: 0.4
`

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mahamantri.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidConfig(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Opencode.ServerURL != "http://localhost:4096" || c.Senapati.RotationThreshold != 0.4 {
		t.Errorf("Load() = %+v", c)
	}
}

// Regression: state/registry/log used to resolve against the launch
// directory, so running from anywhere else silently created a new Senapati.
func TestRelativePathsResolveAgainstConfigDirNotCwd(t *testing.T) {
	path := writeConfig(t, validYAML)
	dir := filepath.Dir(path)
	t.Chdir(t.TempDir())

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "senapati.json"); c.State.SenapatiFile != want {
		t.Errorf("senapatiFile = %q, want %q", c.State.SenapatiFile, want)
	}
	if want := filepath.Join(dir, "registry.json"); c.Attention.RegistryFile != want {
		t.Errorf("registryFile = %q, want %q", c.Attention.RegistryFile, want)
	}
	if want := filepath.Join(dir, "mahamantri.log"); c.LogFile() != want {
		t.Errorf("LogFile() = %q, want %q", c.LogFile(), want)
	}
}

func TestAbsolutePathsAreLeftAlone(t *testing.T) {
	c, err := Load(writeConfig(t, strings.Replace(validYAML, `"./senapati.json"`, `"/var/lib/mm/senapati.json"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.State.SenapatiFile != "/var/lib/mm/senapati.json" {
		t.Errorf("senapatiFile = %q", c.State.SenapatiFile)
	}
}

func TestUnknownKeyIsRejectedNotSilentlyIgnored(t *testing.T) {
	typo := strings.Replace(validYAML, "rotationThreshold", "rotationThreshhold", 1)
	if _, err := Load(writeConfig(t, typo)); err == nil {
		t.Error("a misspelled key should fail loudly")
	}
}

func TestLoadMissingRequiredFieldFails(t *testing.T) {
	if _, err := Load(writeConfig(t, "opencode:\n  serverURL: http://localhost:4096\n  password: pw\n")); err == nil {
		t.Error("Load() with missing required fields should fail validation")
	}
}

func TestServerURLMustBeAURL(t *testing.T) {
	bad := strings.Replace(validYAML, "http://localhost:4096", "localhost:4096", 1)
	if _, err := Load(writeConfig(t, bad)); err == nil {
		t.Error("a serverURL without a scheme should be rejected")
	}
}

func TestEnvFallbackFillsEmptySecrets(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "from-env")
	t.Setenv("LINEAR_SIGNING_SECRET", "also-from-env")
	y := strings.NewReplacer("  password: secret-pw\n", "", "  signingSecret: lin_wh_test\n", "").Replace(validYAML)
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Opencode.Password != "from-env" || c.Linear.SigningSecret != "also-from-env" {
		t.Errorf("Load() = %+v, want env fallbacks applied", c)
	}
}

func TestWatchReportsChangesAndBrokenEdits(t *testing.T) {
	path := writeConfig(t, validYAML)
	type result struct {
		cfg Config
		err error
	}
	got := make(chan result, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, path, 5*time.Millisecond, func(c Config, err error) { got <- result{c, err} })
	time.Sleep(30 * time.Millisecond)

	os.WriteFile(path, []byte(strings.Replace(validYAML, "secret-pw", "new-pw", 1)), 0o644)
	select {
	case r := <-got:
		if r.err != nil || r.cfg.Opencode.Password != "new-pw" {
			t.Errorf("after edit: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not report the changed file")
	}

	os.WriteFile(path, []byte("opencode: [not valid"), 0o644)
	select {
	case r := <-got:
		if r.err == nil {
			t.Error("a broken edit should be reported as an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not report the broken file")
	}
}

func TestAttentionListenAddrMustBeLoopback(t *testing.T) {
	for _, addr := range []string{":4097", "0.0.0.0:4097", "192.168.1.5:4097", "example.com:4097"} {
		y := strings.Replace(validYAML, `"127.0.0.1:4097"`, `"`+addr+`"`, 1)
		if _, err := Load(writeConfig(t, y)); err == nil {
			t.Errorf("%s should be rejected: the gateway exposes opencode's credentials", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:4097", "localhost:4097", "[::1]:4097"} {
		y := strings.Replace(validYAML, `"127.0.0.1:4097"`, `"`+addr+`"`, 1)
		if _, err := Load(writeConfig(t, y)); err != nil {
			t.Errorf("%s should be accepted: %v", addr, err)
		}
	}
}

func TestModelPolicyAndSainikDefaultsLoad(t *testing.T) {
	y := validYAML + `
sainik:
  directory: /repos
  defaultModel: opencode-go/deepseek-v4.1-flash
models:
  - id: opencode-go/deepseek-v4.1-flash
    credits: plentiful
    use: routine work
  - id: openai/gpt-6-sol
    credits: scarce
`
	c, err := Load(writeConfig(t, y))
	if err != nil {
		t.Fatal(err)
	}
	if c.Sainik.DefaultModel != "opencode-go/deepseek-v4.1-flash" || len(c.Models) != 2 || c.Models[1].Credits != "scarce" {
		t.Errorf("Load() = %+v", c)
	}
}

func TestMalformedModelReferencesAreRejected(t *testing.T) {
	for _, y := range []string{
		validYAML + "sainik:\n  defaultModel: gpt-6-sol\n",
		validYAML + "models:\n  - id: gpt-6-sol\n",
	} {
		if _, err := Load(writeConfig(t, y)); err == nil {
			t.Errorf("a model without a provider should be rejected:\n%s", y)
		}
	}
}

// The webhook filter depends on knowing who Senapati is on Linear: without
// its user id its own comments come back as pings (the noise the user saw).
func TestLinearIdentityIsRequired(t *testing.T) {
	for _, key := range []string{"botUserID", "botHandle"} {
		var kept []string
		for _, line := range strings.Split(validYAML, "\n") {
			if !strings.Contains(line, key+":") {
				kept = append(kept, line)
			}
		}
		if _, err := Load(writeConfig(t, strings.Join(kept, "\n"))); err == nil || !strings.Contains(err.Error(), "linear."+key) {
			t.Errorf("without %s: err = %v", key, err)
		}
	}
}

func TestLinearThreadsFileDefaultsNextToTheConfig(t *testing.T) {
	path := writeConfig(t, validYAML)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "mahamantri-linear-threads.json"); c.State.LinearThreadsFile != want {
		t.Errorf("linearThreadsFile = %q, want %q", c.State.LinearThreadsFile, want)
	}
}

func TestExtraPermissions(t *testing.T) {
	ok := strings.Replace(validYAML, "  rotationThreshold: 0.4", "  rotationThreshold: 0.4\n  extraPermissions:\n    - {action: github_*, resource: \"*\", effect: allow}", 1)
	c, err := Load(writeConfig(t, ok))
	if err != nil || len(c.Senapati.ExtraPermissions) != 1 || c.Senapati.ExtraPermissions[0].Action != "github_*" {
		t.Fatalf("Load = %+v, %v", c.Senapati.ExtraPermissions, err)
	}
	ask := strings.Replace(ok, "effect: allow", "effect: ask", 1)
	if _, err := Load(writeConfig(t, ask)); err == nil || !strings.Contains(err.Error(), "extraPermissions[0]") {
		t.Errorf("an ask rule would hang Senapati and must be rejected: %v", err)
	}
}
