package knowledge

import (
	"encoding/json"
	"strings"
	"testing"
)

func good() Config {
	return Config{"homelab-handbook": {
		Kind: "files", Volume: "flotilla-knowledge", Paths: []string{"docs/recipes"},
		Maps: map[string]map[string]string{
			"by_service":   {"litellm": "docs/recipes/litellm.md"},
			"by_case_kind": {"failed_backup": "docs/recipes/backup.md"},
		}}}
}

func TestValidate(t *testing.T) {
	if err := good().Validate(); err != nil {
		t.Fatalf("a good section: %v", err)
	}
	cases := map[string]struct {
		edit func(Config)
		want string
	}{
		"kind":      {func(c Config) { s := c["homelab-handbook"]; s.Kind = "git"; c["homelab-handbook"] = s }, "files is the only kind"},
		"volume":    {func(c Config) { s := c["homelab-handbook"]; s.Volume = ""; c["homelab-handbook"] = s }, "a Docker volume name"},
		"no paths":  {func(c Config) { s := c["homelab-handbook"]; s.Paths = nil; c["homelab-handbook"] = s }, "at least one path"},
		"glob path": {func(c Config) { s := c["homelab-handbook"]; s.Paths = []string{"docs/**"}; c["homelab-handbook"] = s }, "no glob"},
		"dotdot path": {func(c Config) {
			s := c["homelab-handbook"]
			s.Paths = []string{"docs/../etc"}
			c["homelab-handbook"] = s
		}, "relative and clean"},
		"absolute path": {func(c Config) { s := c["homelab-handbook"]; s.Paths = []string{"/etc"}; c["homelab-handbook"] = s }, "relative and clean"},
		"no maps":       {func(c Config) { s := c["homelab-handbook"]; s.Maps = nil; c["homelab-handbook"] = s }, "at least one query"},
		"bad key":       {func(c Config) { c["homelab-handbook"].Maps["by_service"]["a*b"] = "docs/recipes/x.md" }, "key"},
		"escaping value": {func(c Config) {
			c["homelab-handbook"].Maps["by_service"]["x"] = "docs/recipes/../../secret"
		}, "relative and clean"},
		"value not copied": {func(c Config) { c["homelab-handbook"].Maps["by_service"]["x"] = "infra/x.md" }, "not under a declared path"},
		"bad source name":  {func(c Config) { c["Bad_Name"] = c["homelab-handbook"] }, "name: lower-case"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := good()
			tc.edit(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSubsetRefusesWhatIsNotDeclared(t *testing.T) {
	c := good()
	for name, tc := range map[string]struct {
		use  Use
		want string
	}{
		"undeclared source": {Use{Sources: []string{"vendor-docs"}, Queries: []string{"by_service"}}, `no source "vendor-docs"`},
		"undeclared query":  {Use{Sources: []string{"homelab-handbook"}, Queries: []string{"by_anything"}}, `source "homelab-handbook" has none of the queries`},
		"one query missing": {Use{Sources: []string{"homelab-handbook"}, Queries: []string{"by_service", "by_anything"}}, `query "by_anything" is in none`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Subset(tc.use); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestSubsetAndJSONCarryOnlyTheDeclared(t *testing.T) {
	sub, err := good().Subset(Use{Sources: []string{"homelab-handbook"}, Queries: []string{"by_service"}})
	if err != nil {
		t.Fatal(err)
	}
	js, err := sub.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sources map[string]struct {
			Kind string
			Maps map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Sources["homelab-handbook"].Maps
	if len(m) != 1 || m["by_service"]["litellm"] != "docs/recipes/litellm.md" {
		t.Fatalf("maps: %v", m)
	}
	for _, leak := range []string{"flotilla-knowledge", "by_case_kind", "paths", "volume"} {
		if strings.Contains(js, leak) {
			t.Errorf("the run's JSON carries %q: %s", leak, js)
		}
	}
}
