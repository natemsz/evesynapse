// Command modbuild chooses which optional modules an EveSynapse build
// contains. A module is compiled in when internal/app/modules_enabled.go
// imports its package, so choosing modules means writing that file; this
// tool writes it, resolving what each chosen module requires, keeping the
// registration order the menus depend on, and saying which ESI scopes the
// choice will ask players for.
//
//	go run ./cmd/modbuild -list
//	    Every module, what it requires and the scopes it declares.
//
//	go run ./cmd/modbuild -ids
//	    Just the module ids, one per line (for scripts).
//
//	go run ./cmd/modbuild -modules fitting,doctrines
//	    Print the file for that selection (the modules it requires come
//	    along), then the scopes the build would request. Nothing is
//	    written without -write.
//
//	go run ./cmd/modbuild -modules fitting,doctrines -write
//	go run ./cmd/modbuild -all -write
//	    Write internal/app/modules_enabled.go, then:
//
//	        go build ./cmd/evesynapse
//
//	go run ./cmd/modbuild -all -check
//	    Exit non-zero when the file on disk is not what the selection
//	    would produce (CI runs this for the default, all-modules build).
//
//	-core-only is the empty selection. The separate `-tags core_only` build
//	tag still works and compiles none regardless of this file.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"sort"
	"strings"

	"evesynapse/internal/app/module"
	"evesynapse/internal/app/modulecatalog"
)

const defaultFile = "internal/app/modules_enabled.go"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "modbuild:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("modbuild", flag.ContinueOnError)
	fs.SetOutput(stderr)
	list := fs.Bool("list", false, "list every module")
	listIDs := fs.Bool("ids", false, "print the module ids, one per line")
	modules := fs.String("modules", "", "comma-separated module ids to build in")
	all := fs.Bool("all", false, "build in every module")
	none := fs.Bool("core-only", false, "build in no module")
	write := fs.Bool("write", false, "write the file instead of printing it")
	check := fs.Bool("check", false, "fail when the file on disk differs from the selection")
	file := fs.String("file", defaultFile, "the file to write or check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *list {
		listModules(stdout)
		return nil
	}
	if *listIDs {
		for _, e := range modulecatalog.All {
			fmt.Fprintln(stdout, e.Module.ID)
		}
		return nil
	}
	chosen := 0
	for _, on := range []bool{*modules != "", *all, *none} {
		if on {
			chosen++
		}
	}
	if chosen != 1 {
		return fmt.Errorf("choose one of -modules, -all or -core-only (or -list); run with -h for the forms")
	}

	var ids []string
	switch {
	case *all:
		for _, e := range modulecatalog.All {
			ids = append(ids, e.Module.ID)
		}
	case *modules != "":
		for _, id := range strings.Split(*modules, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	}
	sel, err := resolve(ids)
	if err != nil {
		return err
	}
	src, err := render(sel.Entries)
	if err != nil {
		return err
	}

	switch {
	case *check:
		have, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		if !bytes.Equal(have, src) {
			return fmt.Errorf("%s is not what this selection produces (run: go run ./cmd/modbuild ... -write)", *file)
		}
		fmt.Fprintf(stdout, "%s matches the selection (%d modules)\n", *file, len(sel.Entries))
		return nil
	case *write:
		if err := os.WriteFile(*file, src, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", *file)
	default:
		stdout.Write(src)
	}
	report(stdout, sel)
	return nil
}

// Selection is a resolved choice of modules.
type Selection struct {
	Entries  []modulecatalog.Entry // in registration order
	Added    []string              // pulled in by Requires, not asked for
	Warnings []string
}

// resolve turns the ids asked for into the modules to build in: unknown ids
// are an error, whatever a chosen module requires comes along, and the
// result keeps the catalog's registration order (the menus depend on it).
func resolve(ids []string) (Selection, error) {
	asked := map[string]bool{}
	for _, id := range ids {
		if _, ok := modulecatalog.ByID(id); !ok {
			return Selection{}, fmt.Errorf("unknown module %q (known: %s)", id, strings.Join(knownIDs(), ", "))
		}
		asked[id] = true
	}
	want := map[string]bool{}
	var add func(id string) error
	add = func(id string) error {
		if want[id] {
			return nil
		}
		e, ok := modulecatalog.ByID(id)
		if !ok {
			return fmt.Errorf("a module requires %q, which does not exist", id)
		}
		want[id] = true
		for _, req := range e.Module.Requires {
			if err := add(req); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := add(id); err != nil {
			return Selection{}, err
		}
	}
	var sel Selection
	for _, e := range modulecatalog.All {
		if !want[e.Module.ID] {
			continue
		}
		sel.Entries = append(sel.Entries, e)
		if !asked[e.Module.ID] {
			sel.Added = append(sel.Added, e.Module.ID)
		}
	}
	sel.Warnings = menuWarnings(sel.Entries)
	return sel, nil
}

func knownIDs() []string {
	ids := make([]string, 0, len(modulecatalog.All))
	for _, e := range modulecatalog.All {
		ids = append(ids, e.Module.ID)
	}
	sort.Strings(ids)
	return ids
}

// menuWarnings says where a chosen module's menu entry follows an entry
// that belongs to a module left out: with its anchor missing it lands at the
// end of its menu instead of beside it.
func menuWarnings(entries []modulecatalog.Entry) []string {
	owner := map[string]string{} // href -> module id, over the whole catalog
	for _, e := range modulecatalog.All {
		for _, sec := range e.Module.Nav {
			for _, n := range sec.Entries {
				owner[n.Href] = e.Module.ID
			}
		}
	}
	chosen := map[string]bool{}
	for _, e := range entries {
		chosen[e.Module.ID] = true
	}
	var out []string
	for _, e := range entries {
		for _, sec := range e.Module.Nav {
			for _, n := range sec.Entries {
				if by, ok := owner[n.After]; n.After != "" && ok && !chosen[by] {
					out = append(out, fmt.Sprintf("%s: the %q menu entry follows %s, which belongs to %s (not built in); it will sit at the end of the %s menu",
						e.Module.ID, n.Label, n.After, by, sec.Section))
				}
			}
		}
	}
	return out
}

// render is the whole of modules_enabled.go for a selection.
func render(entries []modulecatalog.Entry) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("//go:build !core_only\n\npackage app\n\nimport (\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "\t\"evesynapse/internal/app/%s\"\n", e.Pkg)
	}
	b.WriteString("\t\"evesynapse/internal/app/module\"\n)\n\n")
	b.WriteString(`// The build-selected modules. A module is compiled in when this file
// imports its package, and left out when it does not: Go excludes what
// is never imported. The default build enables every module;
// ` + "`go build -tags core_only`" + ` swaps this file for modules_core_only.go
// and enables none. One line per module, in nav order.
//
// Generated by cmd/modbuild: choose modules with
//
//	go run ./cmd/modbuild -modules fitting,doctrines -write
//
// and do not edit by hand (CI checks the default, all-modules file).
func init() {
	registerModules([]module.Module{
`)
	for _, e := range entries {
		fmt.Fprintf(&b, "\t\t%s.Module,\n", e.Pkg)
	}
	b.WriteString("\t})\n}\n")
	return format.Source(b.Bytes())
}

func listModules(w io.Writer) {
	for _, e := range modulecatalog.All {
		m := e.Module
		fmt.Fprintf(w, "%-14s %s\n", m.ID, m.Title)
		if len(m.Requires) > 0 {
			fmt.Fprintf(w, "    requires: %s\n", strings.Join(m.Requires, ", "))
		}
		for _, s := range m.Scopes {
			fmt.Fprintf(w, "    needs     %s\n", s.Scope)
		}
		for _, s := range m.EnhanceScopes {
			fmt.Fprintf(w, "    enhances  %s\n", s.Scope)
		}
	}
}

// report says what a selection builds in and the scopes it declares.
func report(w io.Writer, sel Selection) {
	fmt.Fprintf(w, "\n%d module(s):", len(sel.Entries))
	for _, e := range sel.Entries {
		fmt.Fprintf(w, " %s", e.Module.ID)
	}
	fmt.Fprintln(w)
	if len(sel.Added) > 0 {
		fmt.Fprintf(w, "added because a chosen module requires them: %s\n", strings.Join(sel.Added, ", "))
	}
	for _, warn := range sel.Warnings {
		fmt.Fprintln(w, "warning:", warn)
	}
	needs, enhances := scopeSets(sel.Entries)
	fmt.Fprintf(w, "scopes the selected modules need (%d):\n", len(needs))
	for _, s := range needs {
		fmt.Fprintln(w, "  ", s)
	}
	fmt.Fprintf(w, "scopes they only enhance with (%d):\n", len(enhances))
	for _, s := range enhances {
		fmt.Fprintln(w, "  ", s)
	}
}

// scopeSets is the union of the selected modules' declared scopes: the ones
// a module cannot work without, and the ones that only light up more of it.
// A scope that is needed anywhere is not also listed as an enhancement.
func scopeSets(entries []modulecatalog.Entry) (needs, enhances []string) {
	need := map[string]bool{}
	enh := map[string]bool{}
	for _, e := range entries {
		for _, s := range e.Module.Scopes {
			need[s.Scope] = true
		}
		for _, s := range e.Module.EnhanceScopes {
			enh[s.Scope] = true
		}
	}
	for s := range need {
		needs = append(needs, s)
	}
	for s := range enh {
		if !need[s] {
			enhances = append(enhances, s)
		}
	}
	sort.Strings(needs)
	sort.Strings(enhances)
	return needs, enhances
}

var _ = module.Module{}
