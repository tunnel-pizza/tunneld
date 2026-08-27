package golib_test

import (
	"testing"

	"github.com/cnuss/golib"
	v1 "github.com/cnuss/golib/v1"
)

// TestAliasesAreAliases pins that the root re-exports are type aliases (`=`)
// rather than defined types, so the two spellings of each type are one type
// and values cross freely between code that imports the root and code that
// imports v1.
//
// The Result assignments are the load-bearing half: structs are assignable
// only when at most one side is a defined type, so declaring Result without
// the `=` fails to compile right here. The Builder assignments can't catch the
// same slip — interface assignability is structural, so a defined interface
// type with the same method set stays assignable — and are here to document
// the intended usage.
func TestAliasesAreAliases(t *testing.T) {
	var fromRoot golib.BuilderV1[string] = golib.New[string]()
	var fromV1 v1.Builder[string] = fromRoot
	fromRoot = fromV1

	var resultRoot golib.Result[string] = fromRoot.WithValue("hello").Build()
	var resultV1 v1.Result[string] = resultRoot
	resultRoot = resultV1

	if resultRoot.Value != "hello" {
		t.Errorf("Value = %q, want %q", resultRoot.Value, "hello")
	}
}

// TestFacadeCoversTheCommonPath pins that application code can do the whole
// job — build a value and name the types it holds — importing only the root
// package. The v1 import above exists solely for the alias check.
func TestFacadeCoversTheCommonPath(t *testing.T) {
	var b golib.BuilderV1[int] = golib.New[int]()

	res := b.WithName("count").WithValue(7).Build()
	if res.Name != "count" || res.Value != 7 {
		t.Errorf("Build() = %+v, want {Name:count Value:7}", res)
	}

	if golib.Version() == "" {
		t.Error("Version() = empty, want a derived identifier")
	}
}
