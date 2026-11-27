package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// renderParam checks a value against its param's kind and says how it reads on a command line. A value that a shell
// would not pass through unchanged is refused: the generator quotes nothing.
func renderParam(p Param, v any, flowKinds map[string]string) (string, error) {
	s, err := renderParamValue(p, v, flowKinds)
	if err == nil && p.Arg != "" && strings.HasPrefix(s, "-") {
		return "", fmt.Errorf("%q: a value that follows %s cannot start with a dash (the script would read it as a flag)", s, p.Arg)
	}
	return s, err
}

func renderParamValue(p Param, v any, flowKinds map[string]string) (string, error) {
	safe := func(s string) (string, error) {
		if !shellSafeRe.MatchString(s) {
			return "", fmt.Errorf("%q: letters, digits and . _ / : @ %% + = , - only (the value goes on a command line)", s)
		}
		return s, nil
	}
	str := func() (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%v: a %s is text", v, p.Kind)
		}
		return s, nil
	}
	switch p.Kind {
	case KindRegistryAction:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !actionRe.MatchString(s) {
			return "", fmt.Errorf("%q: a registry action name such as backup.rerun", s)
		}
		return s, nil
	case KindMetricName:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !metricNameRe.MatchString(s) {
			return "", fmt.Errorf("%q: a metric name", s)
		}
		return s, nil
	case KindString:
		s, err := str()
		if err != nil {
			return "", err
		}
		return safe(s)
	case KindURL:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !urlParamRe.MatchString(s) {
			return "", fmt.Errorf("%q: an http or https URL", s)
		}
		return s, nil
	case KindPath:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !pathParamRe.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") {
			return "", fmt.Errorf("%q: a clean absolute path", s)
		}
		return s, nil
	case KindRepo:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !repoNameRe.MatchString(s) {
			return "", fmt.Errorf("%q: a repository such as owner/name", s)
		}
		return s, nil
	case KindRepoFile:
		s, err := str()
		if err != nil {
			return "", err
		}
		if !statePathRe.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") {
			return "", fmt.Errorf("%q: a clean path inside the repository", s)
		}
		return s, nil
	case KindCount:
		n, ok := v.(int)
		if !ok || n < 0 {
			return "", fmt.Errorf("%v: a count, a whole number from 0", v)
		}
		return strconv.Itoa(n), nil
	case KindLabelList:
		l, ok := v.([]any)
		if !ok || len(l) == 0 {
			return "", fmt.Errorf("%v: a list of label names", v)
		}
		var out []string
		for _, e := range l {
			s, ok := e.(string)
			if !ok || !labelNameRe.MatchString(s) {
				return "", fmt.Errorf("%v: %v is not a label name", v, e)
			}
			out = append(out, s)
		}
		return strings.Join(out, ","), nil
	case KindLabelMatch:
		m, ok := v.(map[string]any)
		if !ok || len(m) == 0 {
			return "", fmt.Errorf("%v: a map of label name to match", v)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			s, ok := m[k].(string)
			if !ok || !labelNameRe.MatchString(k) || s == "" {
				return "", fmt.Errorf("%v: label %s needs a text match", v, k)
			}
			out = append(out, k+"="+s)
		}
		return safe(strings.Join(out, ","))
	case KindFlowList:
		l, ok := v.([]any)
		if !ok || len(l) == 0 {
			return "", fmt.Errorf("%v: a list of the fleet's flows", v)
		}
		var kinds []string
		for _, e := range l {
			s, _ := e.(string)
			k, found := flowKinds[s]
			if !found {
				return "", fmt.Errorf("%v: the fleet has no flow %q", v, s)
			}
			if !contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
		return strings.Join(kinds, ","), nil
	}
	return renderWatchParam(p, v)
}

// command is the run's command line: the params' environment, the block's command, the params' flags.
func command(b *Block, fleet string, with map[string]any, flowKinds map[string]string) (string, error) {
	var env, args []string
	for _, p := range b.Params {
		v, given := with[p.Name]
		if !given {
			continue
		}
		s, err := renderParam(p, v, flowKinds)
		if err != nil {
			return "", fmt.Errorf("with %s: %w", p.Name, err)
		}
		if p.Env != "" {
			env = append(env, p.Env+"="+s)
		} else {
			args = append(args, p.Arg+" "+s)
		}
	}
	cmd := strings.ReplaceAll(b.Command, "{{fleet}}", fleet)
	if len(env) > 0 {
		cmd = "env " + strings.Join(env, " ") + " " + cmd
	}
	if len(args) > 0 {
		cmd += " " + strings.Join(args, " ")
	}
	return cmd, nil
}

// render writes one step's files: routine.yaml, v<n>/spec.yaml and the block's texts. The layout and field order are
// the hand-written routines': a generated folder is read like any other.
func (x *expander) render(st FlowStep, es ExpandedStep, flowKinds map[string]string) (map[string][]byte, error) {
	b := es.Block
	fleet := x.ff.Fleet
	files := map[string][]byte{}
	cmd, err := command(b, fleet, st.With, flowKinds)
	if err != nil {
		return nil, err
	}
	var r strings.Builder
	fmt.Fprintf(&r, "fleet: %s\nroutine: %s\n", fleet, st.Name)
	desc := firstNonEmpty(st.Description, b.Description)
	r.WriteString("description: >-\n")
	for _, l := range strings.Split(desc, "\n") {
		r.WriteString("  " + l + "\n")
	}
	fmt.Fprintf(&r, "enabled: true\nworker: %s\n", st.Worker)
	switch b.Role {
	case RoleMonitor, RoleLearn:
		fmt.Fprintf(&r, "trigger:\n  every: %s\n", firstNonEmpty(st.Every, b.Every))
	case RoleAct, RoleStage:
		c := "kind: " + es.CaseKind
		if es.Stage != "" {
			c += ", stage: " + es.Stage
		}
		if st.Case != nil && st.Case.OnRecord != "" {
			c += ", on_record: " + st.Case.OnRecord
		}
		fmt.Fprintf(&r, "trigger:\n  case: {%s}\n", c)
	case RoleVerify:
		fmt.Fprintf(&r, "trigger:\n  check: {verify: %s}\n", es.Verify)
	}
	if contains(b.Emits, "detection") {
		fmt.Fprintf(&r, "detects: [%s]\n", es.CaseKind)
		if b.BriefSection != "" {
			fmt.Fprintf(&r, "brief_section: %s\n", b.BriefSection)
		}
	}
	fmt.Fprintf(&r, "autonomy: %s\n", firstNonEmpty(st.Autonomy, b.Autonomy))
	// A routine that plans an action always carries a cap (the step's, else its block's); only a case trigger reaches policy, so
	// a monitor, a check and a learning routine, which never plan, keep the line only when the step lowers it.
	if st.MaxAutonomy != "" || b.Role == RoleAct || b.Role == RoleStage {
		fmt.Fprintf(&r, "max_autonomy: %s\n", firstNonEmpty(st.MaxAutonomy, b.MaxAutonomy))
	}
	usd := b.BudgetUSD
	if st.BudgetUSD != nil {
		usd = *st.BudgetUSD
	}
	fmt.Fprintf(&r, "budget:\n  day_usd: %s\n", strconv.FormatFloat(usd, 'f', -1, 64))
	if fresh := firstNonEmpty(st.Freshness, b.Freshness); fresh != "" && (b.Role == RoleMonitor || b.Role == RoleLearn) {
		fmt.Fprintf(&r, "freshness: %s\n", fresh)
	}
	files[st.Name+"/"+RoutineFile] = []byte(r.String())

	var s strings.Builder
	fmt.Fprintf(&s, "run:\n  command: %s\n  workdir: .\n  timeout: %s\n", cmd, firstNonEmpty(st.Timeout, b.Timeout))
	tail, err := b.SpecTailText(st.Model, st.Worker)
	if err != nil {
		return nil, err
	}
	s.Write(tail)
	vdir := st.Name + "/v" + strconv.Itoa(es.Version) + "/"
	files[vdir+"spec.yaml"] = []byte(s.String())
	for _, t := range b.Texts {
		raw, err := os.ReadFile(filepath.Join(b.Dir, t))
		if err != nil {
			return nil, err
		}
		files[vdir+t] = raw
	}
	return files, nil
}

// Drift names every way the committed folders differ from the expansion: a file missing, a file that differs (the first
// differing line), and a file in a generated version folder the generator does not write. Empty: they are equal.
func (e *Expansion) Drift(fleetDir string) []string {
	var out []string
	paths := make([]string, 0, len(e.Files))
	for p := range e.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		got, err := os.ReadFile(filepath.Join(fleetDir, filepath.FromSlash(p)))
		switch {
		case err != nil:
			out = append(out, p+": missing (the generator writes it)")
		case string(got) != string(e.Files[p]):
			out = append(out, p+": "+firstDifference(string(got), string(e.Files[p])))
		}
	}
	for _, s := range e.Steps {
		out = append(out, e.structure(fleetDir, s)...)
	}
	listed := readManifest(fleetDir)
	steps := map[string]bool{}
	for _, s := range e.Steps {
		steps[s.Name] = true
	}
	var gone []string
	for n := range listed {
		if !steps[n] {
			gone = append(gone, n)
		}
	}
	sort.Strings(gone)
	for _, n := range gone {
		if _, err := os.Lstat(filepath.Join(fleetDir, n)); err == nil {
			out = append(out, n+": generated by a flow once and no longer a step (remove the folder, then run flotilla flow expand)")
		}
	}
	return append(out, e.collisions(fleetDir)...)
}

var versionFolderRe = regexp.MustCompile(`^v[0-9]+$`)

// structure checks a step's folder beyond its generated files: no symlink in place of the routine folder or its version
// folder, no file beside routine.yaml, no folder that is not a version, and no file in the version folder the generator does not write.
func (e *Expansion) structure(fleetDir string, s ExpandedStep) []string {
	var out []string
	dir := filepath.Join(fleetDir, s.Name)
	if st, err := os.Lstat(dir); err != nil {
		return nil // missing: its files are reported missing
	} else if st.Mode()&os.ModeSymlink != 0 {
		return []string{s.Name + ": a symlink (a routine folder is a real folder)"}
	}
	vdir := filepath.Join(dir, "v"+strconv.Itoa(s.Version))
	if st, err := os.Lstat(vdir); err == nil && st.Mode()&os.ModeSymlink != 0 {
		out = append(out, s.Name+"/v"+strconv.Itoa(s.Version)+": a symlink (a version is a real folder)")
	}
	ents, _ := os.ReadDir(dir)
	for _, de := range ents {
		switch {
		case de.IsDir() && versionFolderRe.MatchString(de.Name()):
		case de.IsDir():
			out = append(out, s.Name+"/"+de.Name()+": a folder that is not a version, beside generated routine.yaml")
		case de.Name() != RoutineFile:
			out = append(out, s.Name+"/"+de.Name()+": not generated (a routine folder holds routine.yaml and its versions)")
		}
	}
	_ = filepath.WalkDir(vdir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(fleetDir, p)
		if _, ok := e.Files[filepath.ToSlash(rel)]; !ok {
			out = append(out, filepath.ToSlash(rel)+": not generated (the generator does not write it)")
		}
		return nil
	})
	return out
}

// readManifest is the committed list of generated routine folders.
func readManifest(fleetDir string) map[string]bool {
	raw, err := os.ReadFile(filepath.Join(fleetDir, filepath.FromSlash(ManifestFile)))
	m := map[string]bool{}
	if err != nil {
		return m
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			m[l] = true
		}
	}
	return m
}

// collisions names each step whose folder already exists and is not in the committed manifest: a hand-written routine, which a
// flow never takes over by its name alone (list it in flows/.generated to adopt it; its bytes must then equal the expansion).
func (e *Expansion) collisions(fleetDir string) []string {
	listed := readManifest(fleetDir)
	var out []string
	for _, s := range e.Steps {
		if _, err := os.Lstat(filepath.Join(fleetDir, s.Name)); err == nil && !listed[s.Name] {
			out = append(out, s.Name+": a routine folder not in "+ManifestFile+" has this step's name (a hand-written routine; list it there to adopt it)")
		}
	}
	return out
}

func firstDifference(committed, generated string) string {
	a, b := strings.Split(committed, "\n"), strings.Split(generated, "\n")
	for i := 0; i < len(a) || i < len(b); i++ {
		var l, g string
		if i < len(a) {
			l = a[i]
		}
		if i < len(b) {
			g = b[i]
		}
		if i >= len(a) || i >= len(b) || l != g {
			return fmt.Sprintf("differs at line %d: committed %q, generated %q", i+1, clipText(l), clipText(g))
		}
	}
	return "differs"
}

func clipText(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// Write puts the expansion's files under the fleet's folder, creating folders. It deletes nothing.
func (e *Expansion) Write(fleetDir string) error {
	if c := e.collisions(fleetDir); len(c) > 0 {
		return fmt.Errorf("nothing written: %s", strings.Join(c, "; "))
	}
	paths := make([]string, 0, len(e.Files))
	for p := range e.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		full := filepath.Join(fleetDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, e.Files[p], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// flowsPack is what a fleet's flows add to its pack hash: every file under flows/ (path and sha256), nothing for a
// fleet with none, so every existing fleet keeps its hash byte for byte (like knowledgePack).
func flowsPack(fleetDir string) (string, error) {
	root := filepath.Join(fleetDir, FlowsDir)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", nil
	}
	var parts []string
	err := filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		sum := sha256.Sum256(raw)
		parts = append(parts, "flow-file "+filepath.ToSlash(rel)+"="+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(parts)
	return "\n" + strings.Join(parts, "\n"), nil
}
