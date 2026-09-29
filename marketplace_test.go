package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// .claude-plugin/marketplace.json is a source file too: it makes this
// repository a Claude Code marketplace with one plugin in it, and this is its
// test file, beside it. What it pins is everything the file leads to — the
// plugin it names, the skill in that plugin, and the command lines the skill
// tells an agent to type. A skill is instructions a model follows on somebody
// else's machine, so a flag it names that the command no longer has is a run
// that fails there, and nothing here would have said so.

// marketplace is the part of marketplace.json this test reads.
type marketplace struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"plugins"`
}

// launcherWords are the first words the npm launcher keeps for itself.
// tunneld refuses each of them by name, which is what the flag check leans on.
var launcherWords = regexp.MustCompile(`^-(d|k|kd|dk)$`)

// switches matches what Claude Code reads as a boolean in a skill's header:
// true and false, and yes, no, on, off, 1 and 0, in any case.
var switches = regexp.MustCompile(`(?i)^(true|false|yes|no|on|off|1|0)$`)

// flagSpan matches a code span that is a flag and nothing else: `-d`,
// `--no-cache`, `--identity-providers=`.
var flagSpan = regexp.MustCompile("`(--?[a-z][a-z-]*)(?:=[^`]*)?`")

// TestMarketplaceListsThePlugin pins the two mistakes Claude Code's own docs
// name as behind most failed installs from a new marketplace: a source that
// does not lead to the plugin from the marketplace root, and an entry whose
// name is not the one in the plugin's own plugin.json. The first fails at
// install with the path it looked for; the second installs under a name the
// plugin's skills are not prefixed with.
func TestMarketplaceListsThePlugin(t *testing.T) {
	m := marketplaceOf(t)
	if m.Name == "" || len(m.Plugins) == 0 {
		t.Fatalf("marketplace.json = %+v, want a name and at least one plugin", m)
	}
	for _, entry := range m.Plugins {
		if !strings.HasPrefix(entry.Source, "./") || strings.Contains(entry.Source, "..") {
			t.Errorf("plugin %q has source %q, want a ./ path inside the repository", entry.Name, entry.Source)
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		manifestPath := filepath.Join(entry.Source, ".claude-plugin", "plugin.json")
		if err := json.Unmarshal([]byte(read(t, manifestPath)), &manifest); err != nil {
			t.Fatalf("parsing %s: %v", manifestPath, err)
		}
		if manifest.Name != entry.Name {
			t.Errorf("%s names the plugin %q and marketplace.json lists it as %q", manifestPath, manifest.Name, entry.Name)
		}
		if len(skillsOf(t, entry.Source)) == 0 {
			t.Errorf("plugin %q has no skills/<name>/SKILL.md, so it gives an agent nothing", entry.Name)
		}
	}
}

// TestSkillsSayWhenToUseThem pins the header a model picks a skill by. Claude
// Code reads it only when the opening --- is the file's first line, loads a
// skill whose header does not parse with no fields at all, and cuts the
// description at 1,536 characters in the listing, so a trigger past that is
// one the model never reads. The name is the directory's, so the command is
// the one the tree says: /tunneld:share for skills/share.
//
// A switch in the header is one of the words Claude Code documents as a
// boolean. `claude plugin validate` passes a misspelt one without a word, and
// that matters most for disable-model-invocation: /tunneld:session puts this
// conversation on a public URL, and a true nobody can read is a skill the
// model may start on its own.
func TestSkillsSayWhenToUseThem(t *testing.T) {
	for _, entry := range marketplaceOf(t).Plugins {
		for _, skill := range skillsOf(t, entry.Source) {
			fields := frontmatter(t, skill)
			if want := filepath.Base(filepath.Dir(skill)); fields["name"] != want {
				t.Errorf("%s: name = %q, want %q, its directory", skill, fields["name"], want)
			}
			switch description := fields["description"]; {
			case description == "":
				t.Errorf("%s has no description, so no model will pick it", skill)
			case len(description) > 1536:
				t.Errorf("%s: description is %d characters, and the listing keeps 1,536", skill, len(description))
			}
			for _, key := range []string{"disable-model-invocation", "user-invocable"} {
				if value, ok := fields[key]; ok && !switches.MatchString(value) {
					t.Errorf("%s: %s = %q, which Claude Code does not read as true or false", skill, key, value)
				}
			}
		}
	}
}

// TestSkillsTypeOnlyFlagsThatExist pins every flag a skill puts on a tunneld
// command line, or names on its own in a code span. A long flag is one the
// command has. A short one is one of the npm launcher's words, proved by
// asking the command, which refuses each of them by name and says where it
// belongs; asked with --help behind it, so a command that had grown a flag of
// that name would answer with its help rather than mint a tunnel.
func TestSkillsTypeOnlyFlagsThatExist(t *testing.T) {
	cmd := v1alpha1.New().Command()
	for _, entry := range marketplaceOf(t).Plugins {
		for _, skill := range skillsOf(t, entry.Source) {
			named := flagsIn(read(t, skill))
			if len(named) == 0 {
				t.Errorf("%s names no flags, so this test pins nothing there", skill)
			}
			for _, flag := range named {
				switch {
				case strings.HasPrefix(flag, "--"):
					if cmd.Flag(strings.TrimPrefix(flag, "--")) == nil {
						t.Errorf("%s names %s, which tunneld does not have", skill, flag)
					}
				case !launcherWords.MatchString(flag):
					t.Errorf("%s names %s, which is neither a tunneld flag nor the npm launcher's", skill, flag)
				default:
					launcher := v1alpha1.New(v1alpha1.WithStdout(io.Discard), v1alpha1.WithStderr(io.Discard)).Command()
					launcher.SetArgs([]string{flag, "--help"})
					if err := launcher.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "npm launcher") {
						t.Errorf("%s names %s as the npm launcher's, and tunneld answers it with %v", skill, flag, err)
					}
				}
			}
		}
	}
}

// marketplaceOf is .claude-plugin/marketplace.json, parsed.
func marketplaceOf(t *testing.T) marketplace {
	t.Helper()
	var m marketplace
	if err := json.Unmarshal([]byte(read(t, ".claude-plugin/marketplace.json")), &m); err != nil {
		t.Fatalf("parsing marketplace.json: %v", err)
	}
	return m
}

// skillsOf is every SKILL.md in a plugin, where Claude Code looks for them:
// one directory per skill under skills/.
func skillsOf(t *testing.T, plugin string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(plugin, "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatalf("listing %s's skills: %v", plugin, err)
	}
	return found
}

// frontmatter is a SKILL.md's header as its top-level key: value pairs. Read
// by hand, for the reason requirements reads go.mod by hand: a YAML parser
// would be a dependency of its own for one test to credit.
func frontmatter(t *testing.T, path string) map[string]string {
	t.Helper()
	rest, ok := strings.CutPrefix(read(t, path), "---\n")
	if !ok {
		t.Fatalf("%s does not open with ---, so Claude Code reads it as a body with no header", path)
	}
	header, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatalf("%s: the header is never closed", path)
	}
	fields := map[string]string{}
	for line := range strings.Lines(header) {
		if key, value, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(key, " ") {
			fields[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return fields
}

// flagsIn is every flag a skill types: the leading words of each tunneld
// command line, up to its first origin, and every code span that is a flag
// on its own. A program's own flags come after its name and are the
// program's, so `npx tunneld claude --continue` names none.
func flagsIn(skill string) []string {
	var flags []string
	add := func(flag string) {
		if flag, _, _ = strings.Cut(flag, "="); !slices.Contains(flags, flag) {
			flags = append(flags, flag)
		}
	}
	for rest := skill; ; {
		_, after, ok := strings.Cut(rest, "npx tunneld")
		if !ok {
			break
		}
		rest = after
		line, _, _ := strings.Cut(after, "\n")
		line, _, _ = strings.Cut(line, "`")
		for _, word := range strings.Fields(line) {
			if !strings.HasPrefix(word, "-") {
				break
			}
			add(word)
		}
	}
	for _, span := range flagSpan.FindAllStringSubmatch(skill, -1) {
		add(span[1])
	}
	return flags
}
