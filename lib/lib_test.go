// The tests for lib.go. This file is `package lib_test`, the
// outside-the-package view, because the façade has nothing unexported worth
// reaching — the version stamp it used to own now lives in v1alpha1 with its
// resolution. The godoc examples are the one other test file for this source,
// by the exception CONTRIBUTING records for the example_test.go idiom.
package lib_test

import (
	"testing"

	"github.com/tunnel-pizza/tunneld/lib"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// TestAliasIsAnAlias pins that the façade re-export is a type alias (`=`)
// rather than a defined type, so the two spellings are one type and a builder
// crosses freely between code that imports lib and code that imports v1.
func TestAliasIsAnAlias(t *testing.T) {
	var fromLib lib.BuilderV1 = lib.New()
	var fromV1 v1.Builder = fromLib
	fromLib = fromV1

	if fromLib.Name() != v1.CommandName {
		t.Errorf("Name() = %q, want %q", fromLib.Name(), v1.CommandName)
	}
}

// TestFacadeCoversTheCommonPath pins that application code can do the whole
// job — configure the builder and get a runnable command — importing only the
// façade. The v1 import above exists solely for the alias check.
func TestFacadeCoversTheCommonPath(t *testing.T) {
	cmd := lib.New().
		WithName("expose").
		WithURL("http://localhost:3000").
		WithProvider(lib.DefaultProvider).
		Build()

	if cmd == nil {
		t.Fatal("Build() = nil, want a command")
	}
	if got, want := cmd.Name(), "expose"; got != want {
		t.Errorf("cmd.Name() = %q, want %q", got, want)
	}
	if lib.Version() == "" {
		t.Error("Version() = empty, want a derived identifier")
	}
	if lib.VersionLine() == "" {
		t.Error("VersionLine() = empty, want a banner")
	}
}
