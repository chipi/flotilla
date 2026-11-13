package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The stylesheet's colour tokens equal DESIGN.md's frontmatter: same names, same values, none extra.
func TestColoursMatchDesignMD(t *testing.T) {
	doc, err := os.ReadFile("../../DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(doc), "\n---\n", 2)
	if len(parts) != 2 {
		t.Fatal("DESIGN.md has no frontmatter")
	}
	block := regexp.MustCompile(`(?s)\ncolors:\n(.*?)\n[a-z]+:\n`).FindStringSubmatch(parts[0])
	if block == nil {
		t.Fatal("no colors block in DESIGN.md's frontmatter")
	}
	want := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z0-9-]+): "(#[0-9A-Fa-f]{6})"$`).FindAllStringSubmatch(block[1], -1) {
		want[m[1]] = strings.ToUpper(m[2])
	}
	if len(want) < 20 {
		t.Fatalf("only %d colours read from DESIGN.md", len(want))
	}

	css, err := Static.ReadFile("static/flotilla.css")
	if err != nil {
		t.Fatal(err)
	}
	root := regexp.MustCompile(`(?s):root \{(.*?)\n\}`).FindSubmatch(css)
	if root == nil {
		t.Fatal("no :root block in flotilla.css")
	}
	got := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z0-9-]+): (#[0-9A-Fa-f]{6});`).FindAllSubmatch(root[1], -1) {
		got[string(m[1])] = strings.ToUpper(string(m[2]))
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("--%s: DESIGN.md %s, flotilla.css %q", name, v, got[name])
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("--%s is in flotilla.css but not in DESIGN.md", name)
		}
	}
}
