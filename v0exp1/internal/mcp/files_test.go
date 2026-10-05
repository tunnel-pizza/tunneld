package mcp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFiles pins put_file and get_file on a program origin: bytes round
// trip, the mode is applied, and the refusals — relative path, directory,
// missing file, bad base64, a container — are tool errors with nothing
// written.
func TestFiles(t *testing.T) {
	dir := t.TempDir()
	program := []Origin{{Name: "exec:///bin/sh", Kind: KindProgram, Spawner: &fakeSpawner{}}}
	container := []Origin{{Name: "attach://dockerd/web", Kind: KindContainer}}
	content := base64.StdEncoding.EncodeToString([]byte("hello\x00world"))

	t.Run("round trip with a mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no 0600")
		}
		cs := connect(t, program)
		path := filepath.Join(dir, "a.bin")
		var put putFileOut
		if msg := call(t, cs, "put_file", map[string]any{"n": 0, "path": path, "content_base64": content, "mode": "0600"}, &put); msg != "" {
			t.Fatal(msg)
		}
		if put.Bytes != 11 {
			t.Errorf("put_file bytes = %d, want 11", put.Bytes)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("stat = %v, %v; want mode 0600", st, err)
		}
		var got getFileOut
		if msg := call(t, cs, "get_file", map[string]any{"n": 0, "path": path}, &got); msg != "" {
			t.Fatal(msg)
		}
		if got.ContentBase64 != content || got.Bytes != 11 || got.Mode != "0600" {
			t.Errorf("get_file = %+v", got)
		}
	})

	t.Run("a mode is applied to a file that was already there", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows has no 0600")
		}
		cs := connect(t, program)
		path := filepath.Join(dir, "existing")
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if msg := call(t, cs, "put_file", map[string]any{"n": 0, "path": path, "content_base64": content, "mode": "0600"}, nil); msg != "" {
			t.Fatal(msg)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("stat = %v, %v; want mode 0600", st, err)
		}
	})

	for name, tc := range map[string]struct {
		origins []Origin
		tool    string
		args    map[string]any
		wantMsg string
		unix    bool
	}{
		"a relative path is refused": {program, "put_file", map[string]any{"n": 0, "path": "rel.txt", "content_base64": content}, `path "rel.txt" is not absolute`, false},
		"a directory is refused":     {program, "get_file", map[string]any{"n": 0, "path": dir}, "is a directory", false},
		"a directory is not written": {program, "put_file", map[string]any{"n": 0, "path": dir, "content_base64": content}, "is a directory", true},
		"a missing file is refused":  {program, "get_file", map[string]any{"n": 0, "path": filepath.Join(dir, "nope")}, "no such file", true},
		"a container is refused":     {container, "get_file", map[string]any{"n": 0, "path": "/etc/hostname"}, "origin 0 cannot run a program: it is a container origin", false},
		"bad base64 is refused":      {program, "put_file", map[string]any{"n": 0, "path": filepath.Join(dir, "b"), "content_base64": "!!"}, "content_base64", false},
		"a mode that is not octal":   {program, "put_file", map[string]any{"n": 0, "path": filepath.Join(dir, "c"), "content_base64": content, "mode": "rw"}, `mode "rw" is not octal`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.unix && runtime.GOOS == "windows" {
				t.Skip("the error text is the platform's")
			}
			cs := connect(t, tc.origins)
			msg := call(t, cs, tc.tool, tc.args, nil)
			if msg == "" || !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("%s = %q, want an error containing %q", tc.tool, msg, tc.wantMsg)
			}
		})
	}
	for _, name := range []string{"rel.txt", "b", "c"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("a refused put_file wrote %s", name)
		}
	}
}
