package exec

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDecode pins the data: URIs an attachment takes: base64, padded or
// not, and percent-encoded text, with or without a media type; anything
// else is ErrBadAttachment.
func TestDecode(t *testing.T) {
	for uri, want := range map[string]string{
		"data:text/x-shellscript;base64,ZWNobyBoaQo=": "echo hi\n",
		"data:text/x-shellscript;base64,ZWNobyBoaQo":  "echo hi\n",
		"data:;base64,aGk=":                           "hi",
		"data:,hello%20world+%25":                     "hello world+%",
		"data:text/plain;charset=utf-8,a%0Ab":         "a\nb",
		"data:,":                                      "",
	} {
		got, err := decode(uri)
		if err != nil || string(got) != want {
			t.Errorf("decode(%q) = %q, %v; want %q", uri, got, err, want)
		}
	}
	for _, uri := range []string{"", "echo hi", "https://example.com/probe.sh", "data:text/plain;base64", "data:;base64,!!!", "data:,%zz"} {
		if _, err := decode(uri); !errors.Is(err, ErrBadAttachment) {
			t.Errorf("decode(%q) = %v, want ErrBadAttachment", uri, err)
		}
	}
}

// TestPrepare pins what is checked before anything is written: an absolute
// path that is not a directory, a data: URI, an octal mode (0600 when left
// out), and at most 1 MiB in all; the first attachment that fails is named.
func TestPrepare(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	files, err := prepare([]Attachment{
		{Path: at("probe.sh"), Data: "data:,echo%20hi"},
		{Path: at("x/run.sh"), Data: "data:;base64,aGk=", Mode: "0755"},
		{Path: at("y"), Data: "data:,", Mode: "644"},
	})
	if err != nil {
		t.Fatalf("prepare = %v", err)
	}
	want := []file{{at("probe.sh"), []byte("echo hi"), 0o600}, {at("x/run.sh"), []byte("hi"), 0o755}, {at("y"), []byte{}, 0o644}}
	for i := range want {
		if i >= len(files) || files[i].path != want[i].path || !bytes.Equal(files[i].data, want[i].data) || files[i].mode != want[i].mode {
			t.Errorf("prepare()[%d] = %+v, want %+v", i, files, want[i])
		}
	}
	big := "data:;base64," + strings.Repeat("A", maxAttachments/3*4+8)
	for name, tc := range map[string]struct {
		as   []Attachment
		says string
	}{
		"a relative path": {[]Attachment{{Path: at("ok"), Data: "data:,"}, {Path: "probe.sh", Data: "data:,"}}, `attachment 2: "probe.sh" is not an absolute path`},
		"no path":         {[]Attachment{{Data: "data:,"}}, `attachment 1: "" is not an absolute path`},
		"a directory":     {[]Attachment{{Path: dir, Data: "data:,"}}, "attachment 1: " + dir + " is a directory"},
		"not a data URI":  {[]Attachment{{Path: at("a"), Data: "echo hi"}}, "attachment 1: not a data: URI"},
		"a bad mode":      {[]Attachment{{Path: at("a"), Data: "data:,", Mode: "rwx"}}, `attachment 1: mode "rwx" is not octal`},
		"a mode too wide": {[]Attachment{{Path: at("a"), Data: "data:,", Mode: "4755"}}, `attachment 1: mode "4755" is not octal`},
		"more than a MiB": {[]Attachment{{Path: at("a"), Data: big}}, "more than 1 MiB"},
		"more, together":  {[]Attachment{{Path: at("a"), Data: "data:;base64," + strings.Repeat("A", maxAttachments/3*4/2+8)}, {Path: at("b"), Data: "data:;base64," + strings.Repeat("A", maxAttachments/3*4/2+8)}}, "more than 1 MiB"},
	} {
		_, err := prepare(tc.as)
		if !errors.Is(err, ErrBadAttachment) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: prepare = %v, want ErrBadAttachment saying %q", name, err, tc.says)
		}
	}
}

// TestWrite pins how attachments land: missing directories made, a file
// already there replaced, the mode applied, and no temporary file left
// beside them.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []file{{old, []byte("echo new\n"), 0o600}, {filepath.Join(dir, "a", "b", "run.sh"), []byte("hi"), 0o755}}
	if err := write(files); err != nil {
		t.Fatalf("write = %v", err)
	}
	for _, f := range files {
		got, err := os.ReadFile(f.path)
		if err != nil || !bytes.Equal(got, f.data) {
			t.Errorf("%s holds %q, %v; want %q", f.path, got, err, f.data)
		}
		if runtime.GOOS == "windows" {
			continue
		}
		if info, err := os.Stat(f.path); err != nil || info.Mode().Perm() != f.mode {
			t.Errorf("%s mode %v, %v; want %v", f.path, info.Mode().Perm(), err, f.mode)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "a")); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("made directory mode %v, want 0700", info.Mode().Perm())
	}
	var left []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, _ error) error {
		if strings.Contains(d.Name(), ".tunneld-") {
			left = append(left, path)
		}
		return nil
	})
	if len(left) > 0 {
		t.Errorf("left behind %q", left)
	}
}
