package main

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"evesynapse/internal/app/modulecatalog"
)

// TestDefaultFileIsTheAllModulesSelection: the file the default build
// compiles is exactly what the tool writes for every module, so the tool
// and the file cannot drift apart.
func TestDefaultFileIsTheAllModulesSelection(t *testing.T) {
	var ids []string
	for _, e := range modulecatalog.All {
		ids = append(ids, e.Module.ID)
	}
	sel, err := resolve(ids)
	if err != nil {
		t.Fatal(err)
	}
	want, err := render(sel.Entries)
	if err != nil {
		t.Fatal(err)
	}
	have, err := os.ReadFile("../../" + defaultFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(have, want) {
		t.Fatalf("%s is not what `modbuild -all` writes; run: go run ./cmd/modbuild -all -write", defaultFile)
	}
}

// TestCatalogIsComplete: every module package under internal/app that
// declares a Module is in the catalog (a new module that is not listed
// could never be chosen), and every id is unique.
func TestCatalogIsComplete(t *testing.T) {
	seen := map[string]bool{}
	pkgs := map[string]bool{}
	for _, e := range modulecatalog.All {
		if seen[e.Module.ID] {
			t.Errorf("module id %q is in the catalog twice", e.Module.ID)
		}
		seen[e.Module.ID] = true
		pkgs[e.Pkg] = true
	}
	entries, err := os.ReadDir("../../internal/app")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		if !d.IsDir() || d.Name() == "module" || d.Name() == "modulecatalog" {
			continue
		}
		files, _ := os.ReadDir("../../internal/app/" + d.Name())
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			src, err := os.ReadFile("../../internal/app/" + d.Name() + "/" + f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(src, []byte("\nvar Module = module.Module{")) && !pkgs[d.Name()] {
				t.Errorf("package %s declares a Module but is not in internal/app/modulecatalog", d.Name())
			}
		}
	}
}

// TestRequiresComeAlong: choosing a module pulls in what it requires, in
// registration order, and says so.
func TestRequiresComeAlong(t *testing.T) {
	sel, err := resolve([]string{"doctrines"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range sel.Entries {
		got = append(got, e.Module.ID)
	}
	if strings.Join(got, ",") != "fitting,doctrines" {
		t.Errorf("selection = %v, want fitting then doctrines", got)
	}
	if len(sel.Added) != 1 || sel.Added[0] != "fitting" {
		t.Errorf("added = %v, want [fitting]", sel.Added)
	}
}

// TestUnknownModuleIsAnError and the empty selection.
func TestUnknownModuleIsAnError(t *testing.T) {
	if _, err := resolve([]string{"nosuchmodule"}); err == nil || !strings.Contains(err.Error(), "known:") {
		t.Errorf("unknown id error = %v, want one listing the known ids", err)
	}
	sel, err := resolve(nil)
	if err != nil || len(sel.Entries) != 0 {
		t.Fatalf("empty selection = %+v, %v", sel, err)
	}
}

// TestRenderedFileImportsOnlyTheSelection: the generated file parses, and
// imports and registers exactly the chosen modules.
func TestRenderedFileImportsOnlyTheSelection(t *testing.T) {
	sel, err := resolve([]string{"tradeideas", "wallet"})
	if err != nil {
		t.Fatal(err)
	}
	src, err := render(sel.Entries)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "modules_enabled.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("generated file does not parse: %v", err)
	}
	var imports []string
	for _, imp := range f.Imports {
		imports = append(imports, strings.Trim(imp.Path.Value, `"`))
	}
	want := "evesynapse/internal/app/module evesynapse/internal/app/tradeideas evesynapse/internal/app/wallet"
	if strings.Join(imports, " ") != want {
		t.Errorf("imports = %v, want %q", imports, want)
	}
	if !strings.Contains(string(src), "wallet.Module,\n\t\ttradeideas.Module,") {
		t.Errorf("registration order is not the catalog's:\n%s", src)
	}
	if !strings.HasPrefix(string(src), "//go:build !core_only\n") {
		t.Error("the generated file lost its build tag")
	}
}

// TestMenuWarning: a module whose menu entry follows one of a module left
// out is told so.
func TestMenuWarning(t *testing.T) {
	sel, err := resolve([]string{"calendar"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Warnings) != 1 || !strings.Contains(sel.Warnings[0], "/mail/") {
		t.Errorf("warnings = %v, want one about the mail entry", sel.Warnings)
	}
	all, _ := resolve([]string{"calendar", "mail", "wallet"})
	if len(all.Warnings) != 0 {
		t.Errorf("warnings with the anchor chain built in = %v, want none", all.Warnings)
	}
}

// TestScopesAreTheUnionOfDeclarations.
func TestScopesAreTheUnionOfDeclarations(t *testing.T) {
	sel, _ := resolve([]string{"mail", "wallet"})
	needs, enh := scopeSets(sel.Entries)
	if strings.Join(needs, " ") != "esi-mail.organize_mail.v1 esi-mail.read_mail.v1 esi-mail.send_mail.v1 esi-wallet.read_character_wallet.v1" {
		t.Errorf("needs = %v", needs)
	}
	if len(enh) != 0 {
		t.Errorf("enhances = %v", enh)
	}
}
