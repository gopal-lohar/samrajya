package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gopal-lohar/samrajya/mahamantri/config"
	bolt "go.etcd.io/bbolt"
	"gopkg.in/yaml.v3"
)

// Admin commands, for scripts/samrajya: they act on files, never on a
// running mahamantri.

// checkConfig loads and validates a config, and says what it would run.
func checkConfig(path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	var surfaces []string
	if c.LinearOn() {
		surfaces = append(surfaces, "linear")
	}
	if c.TelegramOn() {
		surfaces = append(surfaces, "telegram")
	}
	fmt.Printf("ok: mode %s, surfaces %v, store %s, sessions in %s\n", c.Mode, surfaces, c.Bridge.Store, c.Bridge.MainDirectory)
	return nil
}

// migrateConfig rewrites a config from before surfaces and agents mode:
// bridge.trustedUserIDs moves to linear.trustedUserIDs, and bridge.keepMain
// and agents.enabled go (mahamantri runs the sessions itself). Comments and
// everything else are kept.
func migrateConfig(in, out string) error {
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a YAML mapping", in)
	}
	root := doc.Content[0]
	bridgeNode := mapping(root, "bridge")
	if trusted := take(bridgeNode, "trustedUserIDs"); trusted != nil {
		lin := mapping(root, "linear")
		if lin == nil {
			return errors.New("bridge.trustedUserIDs is set but there is no linear section to move it to")
		}
		if mapping(lin, "trustedUserIDs") == nil && value(lin, "trustedUserIDs") == nil {
			lin.Content = append(lin.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "trustedUserIDs"}, trusted)
		}
	}
	take(bridgeNode, "keepMain")
	take(mapping(root, "agents"), "enabled")
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return checkConfig(out)
}

func value(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapping(m *yaml.Node, key string) *yaml.Node {
	if v := value(m, key); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	return nil
}

// take removes key from mapping m and returns its value.
func take(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return v
		}
	}
	return nil
}

// downgradeStore marks a store schema 1 again, for rolling back to a
// mahamantri from before agents. Schema 2 only added buckets and fields,
// which schema 1 ignores, so every pending message is kept: the old main
// session gets them all, whichever agent they were routed to. A copy of the
// store is kept first.
func downgradeStore(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return fmt.Errorf("%s: %w (stop mahamantri first)", path, err)
	}
	defer db.Close()
	return db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta == nil {
			return fmt.Errorf("%s is not a mahamantri store", path)
		}
		switch v := string(meta.Get([]byte("schema"))); v {
		case "1":
			return nil
		case "2":
		default:
			return fmt.Errorf("store schema %q cannot be downgraded", v)
		}
		if err := tx.CopyFile(path+".schema2.bak", 0o600); err != nil {
			return fmt.Errorf("backing up: %w", err)
		}
		return meta.Put([]byte("schema"), []byte("1"))
	})
}
