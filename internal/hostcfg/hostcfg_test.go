package hostcfg

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T) *Config {
	t.Helper()
	c, err := Load("testdata/mini.json")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGoodConfig(t *testing.T) {
	c := load(t)
	if c.Host != "mini" || len(c.Keys()) != 1 || len(c.Actions) != 4 {
		t.Fatalf("config %+v", c)
	}
}

// The sudoers lines: one per privileged action and enum value, the run-as user, "" for a command with
// no arguments (alone it would allow any), nothing for an unprivileged action.
func TestSudoers(t *testing.T) {
	s, err := load(t).Sudoers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"_flotilla ALL=(root) NOPASSWD: /bin/launchctl kickstart system/com.homelab.db-backup  # backup.rerun_observability",
		"_flotilla ALL=(_dockerhost) NOPASSWD: /usr/local/bin/docker restart grafana  # container.restart",
		"_flotilla ALL=(_dockerhost) NOPASSWD: /usr/local/bin/docker restart victoriametrics  # container.restart",
		`_flotilla ALL=(root) NOPASSWD: /usr/bin/uptime ""  # host.uptime`,
	}
	for _, w := range want {
		if !strings.Contains(s, w+"\n") {
			t.Errorf("sudoers lacks %q:\n%s", w, s)
		}
	}
	if strings.Contains(s, "tail") {
		t.Errorf("an unprivileged action reached sudoers:\n%s", s)
	}
	if n := strings.Count(s, "NOPASSWD"); n != len(want) {
		t.Errorf("%d sudoers lines, want %d", n, len(want))
	}
	if strings.ContainsAny(strings.ReplaceAll(s, `""`, ""), "*?[]!\\") {
		t.Errorf("a wildcard or escape in sudoers:\n%s", s)
	}
}

func TestArgv(t *testing.T) {
	c := load(t)
	got, err := c.Argv("container.restart", map[string]string{"host": "mini", "name": "grafana"})
	if err != nil || strings.Join(got, " ") != "/usr/bin/sudo -n -u _dockerhost /usr/local/bin/docker restart grafana" {
		t.Fatalf("argv %v %v", got, err)
	}
	got, err = c.Argv("logs.tail", map[string]string{"host": "mini", "file": "/tmp/com.homelab.db-backup.log"})
	if err != nil || got[3] != "/tmp/com.homelab.db-backup.log" {
		t.Fatalf("pattern argv %v %v", got, err)
	}
	for name, c2 := range map[string]struct {
		action string
		params map[string]string
	}{
		"not allowed here":         {"rm.everything", map[string]string{"host": "mini"}},
		"another host":             {"backup.rerun_observability", map[string]string{"host": "dgx"}},
		"a missing parameter":      {"container.restart", map[string]string{"host": "mini"}},
		"an extra parameter":       {"backup.rerun_observability", map[string]string{"host": "mini", "force": "1"}},
		"an enum value not listed": {"container.restart", map[string]string{"host": "mini", "name": "postgres"}},
		"an enum value smuggled":   {"container.restart", map[string]string{"host": "mini", "name": "grafana victoriametrics"}},
		"a pattern not matched":    {"logs.tail", map[string]string{"host": "mini", "file": "/etc/sudoers"}},
		"a pattern partly matched": {"logs.tail", map[string]string{"host": "mini", "file": "/tmp/com.homelab.x.log/../../etc/passwd"}},
	} {
		if _, err := c.Argv(c2.action, c2.params); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every rule of ADR-0006 §5 the config enforces, one bad config each.
func TestBadConfigs(t *testing.T) {
	good, _ := os.ReadFile("testdata/mini.json")
	for name, c := range map[string]struct{ from, to string }{
		"root as the user":            {`"user": "_flotilla"`, `"user": "root"`},
		"no ceiling":                  {`"ceiling": "propose"`, `"ceiling": "max"`},
		"a bad key":                   {`"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="`, `"short"`},
		"three keys":                  {`"signing_keys": {"k1"`, `"signing_keys": {"k2": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "k3": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", "k1"`},
		"a relative command":          {`"/usr/bin/tail"`, `"tail"`},
		"sudo without -n":             {`"/usr/bin/sudo", "-n", "/bin/launchctl"`, `"/usr/bin/sudo", "/bin/launchctl"`},
		"sudo with another option":    {`"/usr/bin/sudo", "-n", "/bin/launchctl"`, `"/usr/bin/sudo", "-n", "-E", "/bin/launchctl"`},
		"sudo with -E, not -n":        {`"/usr/bin/sudo", "-n", "/bin/launchctl"`, `"/usr/bin/sudo", "-E", "/bin/launchctl"`},
		"a relative command via sudo": {`"-n", "/bin/launchctl"`, `"-n", "launchctl"`},
		"a wildcard, privileged":      {`"system/com.homelab.db-backup"`, `"system/com.homelab.*"`},
		"a space, privileged":         {`"kickstart"`, `"kickstart -k"`},
		"a separator, privileged":     {`"kickstart"`, `"kickstart,print"`},
		"a pattern, privileged":       {`"name": {"kind": "enum", "values": ["grafana", "victoriametrics"]}`, `"name": {"kind": "pattern", "pattern": "[a-z]+"}`},
		"an enum value with a glob":   {`"values": ["grafana", "victoriametrics"]`, `"values": ["grafana", "victoria*"]`},
		"a partial placeholder":       {`"{file}"`, `"--file={file}"`},
		"an undeclared placeholder":   {`"{file}"`, `"{path}"`},
		"a declared, unused param":    {`"{file}"`, `"/tmp/x"`},
		"this_host substituted":       {`"restart", "{name}"`, `"restart", "{host}"`},
		"a bad pattern":               {`"/tmp/com\\.homelab\\.[a-z-]+\\.log"`, `"(["`},
		"no rate limit":               {`"per_hour": 1,`, `"per_hour": 0,`},
		"no timeout":                  {`"timeout": "30m"`, `"timeout": "0s"`},
		"an unknown field":            {`"per_hour": 1,`, `"per_hour": 1, "shell": true,`},
		"an unknown precondition":     {`{"kind": "launchd_not_running", "args": {"label": "system/com.homelab.db-backup"}}`, `{"kind": "file_absent"}`},
		"a precondition missing args": {`"args": {"label": "system/com.homelab.db-backup"}`, `"args": {"name": "system/com.homelab.db-backup"}`},
		"a precondition, extra args":  {`"args": {"label": "system/com.homelab.db-backup"}`, `"args": {"label": "system/com.homelab.db-backup", "x": "y"}`},
	} {
		body := strings.Replace(string(good), c.from, c.to, 1)
		if body == string(good) {
			t.Fatalf("%s: the test's edit did not apply", name)
		}
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// With SUDOERS_OUT set, writes the fixture's sudoers there, for `visudo -c -f` in a container (the e2e,
// and by hand: the generator's output must be one sudo accepts).
func TestWriteSudoers(t *testing.T) {
	out := os.Getenv("SUDOERS_OUT")
	if out == "" {
		t.Skip("SUDOERS_OUT not set")
	}
	s, err := load(t).Sudoers()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
