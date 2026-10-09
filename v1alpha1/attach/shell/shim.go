package shell

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// shimNotice is what the page says about a program served through the shim:
// a terminal, but not one every program can see.
const shimNotice = "no terminal on this machine — tunneld stands in for one; statically linked and Go programs still see pipes"

// shimEnv is base with what rung 2 adds: the shim preloaded after anything
// already preloaded, the page, the shim's own path for it to stay loaded
// across exec, and the TERM the page's emulator implements with a terminfo
// entry for it, searched before the machine's own.
func shimEnv(base []string, so, page, dir string) []string {
	out := make([]string, 0, len(base)+5)
	preload := so
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "TERM", "TERMINFO_DIRS", "TUNNELD_TTY", "TUNNELD_TTY_SHIM":
			continue
		case "LD_PRELOAD":
			if v != "" {
				preload = v + ":" + so
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"LD_PRELOAD="+preload,
		"TUNNELD_TTY="+page,
		"TUNNELD_TTY_SHIM="+so,
		"TERM=xterm-256color",
		"TERMINFO_DIRS="+filepath.Join(dir, "terminfo")+":",
	)
}

// shimName is the shim's file name, by its content.
func shimName(obj []byte) string {
	sum := sha256.Sum256(obj)
	return "ttyshim-" + hex.EncodeToString(sum[:8]) + ".so"
}
