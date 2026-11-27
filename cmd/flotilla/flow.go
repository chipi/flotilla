package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/chipi/flotilla/internal/registry"
	"github.com/chipi/flotilla/internal/spec"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

// flowCmd is `flotilla flow expand [-check] [-blocks DIR] [-registry FILE]... FLEETDIR` (RFC-0016): offline, with no
// flotillad. It reads FLEETDIR/flows, resolves each step's pinned block, checks the wiring, and writes the routine
// folders (routine.yaml and v<n>/spec.yaml). With -check it writes nothing and fails when the committed folders differ.
// Without -registry the registry's side of the wiring (actions, preconditions, checks) is not checked: flotillad -check does it.
func flowCmd(out io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "expand" {
		return errors.New("usage: flotilla flow expand [-check] [-blocks DIR] [-registry FILE]... FLEETDIR")
	}
	fs := flag.NewFlagSet("flow expand", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "write nothing; fail when the committed folders differ from the expansion")
	blocks := fs.String("blocks", "blocks", "the block library")
	var regs multiFlag
	fs.Var(&regs, "registry", "a registry declaration file (repeatable); without one the actions are not checked")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: flotilla flow expand [-check] [-blocks DIR] [-registry FILE]... FLEETDIR")
	}
	dir := fs.Arg(0)
	var actions spec.Actions
	if len(regs) > 0 {
		reg, err := registry.Load(regs...)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		actions = reg.FlowActions()
	}
	fe, err := spec.ExpandFleet(dir, *blocks, actions)
	if err != nil {
		return err
	}
	if fe == nil {
		return fmt.Errorf("%s has no flows folder", dir)
	}
	drift := fe.Exp.Drift(dir)
	if *check {
		if len(drift) > 0 {
			return fmt.Errorf("the committed folders differ from the expansion of %s/flows:\n  %s", dir, strings.Join(drift, "\n  "))
		}
		fmt.Fprintf(out, "flow expand -check: %s: %d flows, %d routines, committed folders equal the generator's output\n", fe.Fleet, fe.Flows, len(fe.Exp.Steps))
		return nil
	}
	if err := fe.Exp.Write(dir); err != nil {
		return err
	}
	fmt.Fprintf(out, "flow expand: %s: %d flows, %d routines, %d files (%d changed)\n", fe.Fleet, fe.Flows, len(fe.Exp.Steps), len(fe.Exp.Files), len(drift))
	if actions == nil {
		fmt.Fprintln(out, "flow expand: no -registry: the actions, preconditions and checks were not checked")
	}
	return nil
}
