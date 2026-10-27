package refusals

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// texts is what err's reasons say, failing unless err is a Refusal.
func texts(t *testing.T, err error) []string {
	t.Helper()
	var r Refusal
	if !errors.As(err, &r) {
		t.Fatalf("%v is not a refusal", err)
	}
	out := make([]string, len(r))
	for i, reason := range r {
		out[i] = reason.Error()
	}
	return out
}

// TestRefusal pins a refusal: what went wrong, then what to do, as a Go
// error in that order, the reason still found by errors.Is, and no usage
// left out.
func TestRefusal(t *testing.T) {
	wrong := errors.New("what went wrong")
	err := refusal("what to do instead", wrong)
	if got := err.Error(); got != "what went wrong\nwhat to do instead" {
		t.Errorf("Error() = %q", got)
	}
	if !errors.Is(err, wrong) {
		t.Errorf("errors.Is does not find the reason in %v", err)
	}
	if got := texts(t, refusal("", wrong)); len(got) != 1 {
		t.Errorf("refusal with no usage says %q, want the reason alone", got)
	}
	if bad := fmt.Errorf("missing closing )"); !errors.Is(NotAPattern(bad), bad) {
		t.Error("NotAPattern hides the error it wraps")
	}
}

// TestSays pins what each refusal tells the model: what went wrong, then
// what to do instead where there is something, a text each.
func TestSays(t *testing.T) {
	shell, _ := url.Parse("http://localhost:3000")
	for name, tc := range map[string]struct {
		err  error
		says []string
	}{
		"NotImplemented": {NotImplemented("origin_add"), []string{"origin_add is not implemented yet", "This run's origins are the ones tunneld was started with; terminal_sessions lists them"}},
		"SecretRefused":  {SecretRefused("origin_add", "tunnel:origins"), []string{"The tunnel's secret can't call origin_add", "origin_add answers the client that started this tunneld, over stdio, or a sign-in holding tunnel:origins"}},
		"NoTerminal":     {NoTerminal(), []string{"This run serves no terminal: it has no program (exec) or container (attach) to type into", "A terminal is there once tunneld runs a program or attaches a container; terminal_sessions with scheme http lists the web addresses it serves, if any"}},
		"Unlisted":       {Unlisted(fmt.Errorf("names origin 5")), []string{"names origin 5", "terminal_sessions lists the terminals"}},
		"NotATerminal":   {NotATerminal("https://0tabc.tunneld.domain/", v1.Origin{Index: 0, URI: *shell, Scheme: v1.HttpScheme}), []string{"https://0tabc.tunneld.domain/ is not a terminal: origin 0 is http", "terminal_sessions lists the terminals"}},
		"NoScope":        {NoScope("a_tool", "tunnel:exec"), []string{"This sign-in can't call a_tool", "a_tool needs the tunnel:exec scope"}},
		"NotAPattern":    {NotAPattern(fmt.Errorf("missing closing )")), []string{"until is not a regular expression: missing closing )", `until takes RE2 syntax: \s is ASCII only, \p{Zs} matches any space`}},
		"NothingToSend":  {NothingToSend(), []string{"Nothing to send", "send input steps, a command, attachments, or a mix"}},
		"BadAttachment":  {BadAttachment(fmt.Errorf("attachment 1: not a data: URI")), []string{"attachment 1: not a data: URI", "each attachment is an absolute path, a data: URI, and optionally an octal mode, 1 MiB in all"}},
		"Unwritable":     {Unwritable(fmt.Errorf("permission denied")), []string{"Could not write the attachments: permission denied", "nothing was typed; pick a path tunneld's user can write"}},
		"Unsendable":     {Unsendable(fmt.Errorf("%w %q: step 2", attach.ErrUnknownKey, "Hyper")), []string{`attach: unknown key "Hyper": step 2`, "each step is exactly one of text or key"}},
		"UnknownScheme":  {UnknownScheme("ssh"), []string{`"ssh" is not a scheme`, "scheme must be exec, attach or http"}},
	} {
		if got := texts(t, tc.err); !slices.Equal(got, tc.says) {
			t.Errorf("%s says %q, want %q", name, got, tc.says)
		}
	}
}

// TestUnreadable pins the refusal for a read that failed: a cursor it cannot
// use said with what to do, anything else as it is.
func TestUnreadable(t *testing.T) {
	for err, says := range map[error]int{
		attach.ErrStaleCursor:           2,
		attach.ErrBadCursor:             2,
		fmt.Errorf("the session ended"): 1,
	} {
		if got := texts(t, Unreadable(err)); len(got) != says {
			t.Errorf("Unreadable(%v) says %q, want %d texts", err, got, says)
		}
	}
	if got := texts(t, Unreadable(fmt.Errorf("the session ended"))); got[0] != "the session ended" {
		t.Errorf("Unreadable(other) says %q, want it as it is", got)
	}
}
