// Package run is one run's tunnel, from its spec to its end: minted from the
// spec, brought up and put in front of everybody, and waited on until the run
// is over.
//
// Everything a live tunnel does lives here — the addresses on stdout, the map
// on stderr, the bound origins told where they answer from, the provider's
// messages learned, the browser or the console opened, the spec saved, a
// waiting launcher handed back — so what happens to a tunnel over its life
// reads top to bottom in one place. What the run settled before its first
// tunnel (the logger, the origins, what they are bound to, the router's
// address, the credential) the builder hands over with each call, along with
// the collaborators a tunnel is shown through, each behind the narrow interface
// declared below.
package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/cnuss/libtunnel"
	ltv1 "github.com/cnuss/libtunnel/v1"
	"github.com/spf13/cobra"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/cache"
	"github.com/tunnel-pizza/tunneld/v1alpha1/console"
	"github.com/tunnel-pizza/tunneld/v1alpha1/display"
)

// Option configures a RunImpl, at construction or for one call to Run.
type Option = v1.Option[*RunImpl]

// discard is where a run with no logger writes: nowhere.
var discard = slog.New(slog.DiscardHandler)

// Cache is where a tunnel that came up is saved, and where a new spec for the
// run arrives: every spec the cache takes, the run's own saves included.
type Cache interface {
	Save(opts ...cache.Option)
	Spec() <-chan string
}

// Display is how a tunnel is shown: the panel's address, when there is a
// panel; the address as a code for a phone; and a tab or the console opened
// on it.
type Display interface {
	URL(enabled bool, public *url.URL, origins v1.Origins) string
	Open(ctx context.Context, log v1.Logger, opts ...display.Option)
	QR(addr string) ([]string, error)
}

// Console is the screen the run was started on, when there is one to draw a
// frame on.
type Console interface {
	For(bound attach.Bound, streams console.Streams) console.Screen
}

// Motd is where what the provider said with the spec is learned.
type Motd interface {
	Learn(raw []string, log v1.Logger)
}

// Router is what the tunnel forwards to: asked which origins nothing answers
// on, and cancelled once the run is over and its tunnel has drained.
type Router interface {
	Unanswered(ctx context.Context, origins v1.Origins) []int
	Cancel()
}

// Pid hands a detached run back to a launcher waiting on it.
type Pid interface {
	Detach(out *os.File, log v1.Logger) bool
}

// Logs is the run's log file: where a detached run's streams go, and what
// stops showing lines once they do.
type Logs interface {
	File() *os.File
	Detach()
}

// RunImpl is the default Run. Its fields are one call's: New seeds the
// defaults, and each call to Run applies its options to a copy.
type RunImpl struct {
	// spec is what the tunnel is minted from; "" mints fresh.
	spec string
	// tunnel is how a spec becomes a tunnel: libtunnel.From, or a fake.
	tunnel func(spec string) libtunnel.TunnelV1
	// token is the mint's credential, "" for an anonymous mint; userAgent
	// says which tunneld is asking.
	token, userAgent string
	// local is the one address the tunnel forwards to: the router's.
	local *url.URL
	log   *slog.Logger

	// cmd is the command the run belongs to: its streams, and whether
	// somebody is watching them.
	cmd     *cobra.Command
	origins v1.Origins
	bound   attach.Bound
	cache   Cache
	// tracking is what the run settled on, saved beside the spec with the
	// tunnel's hostname added.
	tracking map[string]string

	display   Display
	multiview bool
	console   Console
	motd      Motd
	router    Router
	pid       Pid
	logs      Logs

	// qr prints the address as a code; open is a caller's decision about a
	// tab, nil for none; spinner allows the one shown while the tunnel comes
	// up; hint is what a console with nothing to draw is told.
	qr      bool
	open    *bool
	spinner bool
	hint    string

	// screen is the console a frame was drawn on for an earlier tunnel of
	// this call, nil when none was: that frame is still up, showing the same
	// origins, so a later tunnel keeps it rather than drawing another.
	screen console.Screen
}

// New returns a RunImpl configured by opts: a logger that discards, a spinner
// allowed, and nothing else — the builder hands over the rest with each call.
func New(opts ...Option) *RunImpl {
	return v1.Apply(&RunImpl{log: discard, spinner: true}, opts...)
}

// WithSpec sets the spec the tunnel is minted from; "" mints fresh.
func WithSpec(spec string) Option { return func(r *RunImpl) { r.spec = spec } }

// WithTunnel sets how a spec becomes a tunnel: libtunnel.From, or a fake.
func WithTunnel(from func(spec string) libtunnel.TunnelV1) Option {
	return func(r *RunImpl) { r.tunnel = from }
}

// WithToken sets the mint's credential; "" mints anonymously.
func WithToken(token string) Option { return func(r *RunImpl) { r.token = token } }

// WithUserAgent sets the User-Agent the mint says which tunneld is asking with.
func WithUserAgent(ua string) Option { return func(r *RunImpl) { r.userAgent = ua } }

// WithLocalURL sets the one address the tunnel forwards to: the router's.
func WithLocalURL(local *url.URL) Option { return func(r *RunImpl) { r.local = local } }

// WithLog sets where the run says what it did. Nil keeps the one it has.
func WithLog(log *slog.Logger) Option {
	return func(r *RunImpl) {
		if log != nil {
			r.log = log
		}
	}
}

// WithCommand sets the command the run belongs to: the streams it reports on.
func WithCommand(cmd *cobra.Command) Option { return func(r *RunImpl) { r.cmd = cmd } }

// WithOrigins sets the origins the run exposes, as the operator typed them.
func WithOrigins(origins v1.Origins) Option { return func(r *RunImpl) { r.origins = origins } }

// WithBound sets what the origins are bound to: told the addresses they answer
// on, and how a viewer asks the run to end.
func WithBound(bound attach.Bound) Option { return func(r *RunImpl) { r.bound = bound } }

// WithCache sets where a tunnel that came up is saved.
func WithCache(c Cache) Option { return func(r *RunImpl) { r.cache = c } }

// WithTracking sets what the run settled on, saved beside the spec.
func WithTracking(tracking map[string]string) Option {
	return func(r *RunImpl) { r.tracking = tracking }
}

// WithDisplay sets how the tunnel is shown, and WithMultiview whether there is
// a panel in front of the origins.
func WithDisplay(d Display) Option { return func(r *RunImpl) { r.display = d } }
func WithMultiview(on bool) Option { return func(r *RunImpl) { r.multiview = on } }

// WithConsole sets the screen the run was started on.
func WithConsole(c Console) Option { return func(r *RunImpl) { r.console = c } }

// WithMotd sets where what the provider said is learned.
func WithMotd(m Motd) Option { return func(r *RunImpl) { r.motd = m } }

// WithRouter sets what the tunnel forwards to.
func WithRouter(router Router) Option { return func(r *RunImpl) { r.router = router } }

// WithPid sets what hands a detached run back to a launcher; nil for a run
// that was not registered.
func WithPid(p Pid) Option { return func(r *RunImpl) { r.pid = p } }

// WithLogs sets the run's log file.
func WithLogs(l Logs) Option { return func(r *RunImpl) { r.logs = l } }

// WithQR prints the address as a code for a phone.
func WithQR(on bool) Option { return func(r *RunImpl) { r.qr = on } }

// WithOpen is a caller's decision about a tab; nil is none.
func WithOpen(open *bool) Option { return func(r *RunImpl) { r.open = open } }

// WithSpinner allows the spinner shown while the tunnel comes up; a run with
// its logger on gets the lines instead.
func WithSpinner(on bool) Option { return func(r *RunImpl) { r.spinner = on } }

// WithHint sets what a console with nothing left to draw is told.
func WithHint(hint string) Option { return func(r *RunImpl) { r.hint = hint } }

// Run is one run's tunnel, from its spec to its end: mint, up, wait — and
// again from the top whenever a new spec arrives while it waits, the tunnel
// it has stopped and drained before the next is minted. It returns nil for a
// run told to stop — a signal, or a viewer asking — and the tunnel's error
// when one fails to come up or ends on its own.
func (r *RunImpl) Run(ctx context.Context, opts ...Option) error {
	run := *r
	v1.Apply(&run, opts...)

	// The router outlives the run while its tunnel drains — a request still
	// in flight through the tunnel is still being answered by it — so it
	// comes down once the run is over and whichever tunnel is live then has
	// drained.
	var mu sync.Mutex
	var live libtunnel.TunnelV1
	go func() {
		<-ctx.Done()
		mu.Lock()
		tun := live
		mu.Unlock()
		if tun != nil {
			<-tun.Done()
		}
		run.router.Cancel()
	}()

	spec := run.spec
	for {
		tun, stop := run.mint(ctx, spec)
		mu.Lock()
		live = tun
		mu.Unlock()
		saved, err := run.up(ctx, tun)
		if err != nil {
			stop()
			return err
		}
		next, respec, err := run.wait(ctx, tun, saved)
		if !respec {
			stop()
			return err
		}

		// A new spec is a new tunnel: this one ends, and drains, before the
		// next is minted from it — one tunnel at a time, and never two
		// holding the same hostname.
		run.log.Info("a new spec arrived; replacing the tunnel")
		stop()
		<-tun.Done()
		if ctx.Err() != nil {
			return nil
		}
		spec = next
	}
}

// mint makes spec into the run's tunnel — the stop that ends it and nothing
// else alongside — and hands it back before it is up: up is what waits for
// that.
func (r *RunImpl) mint(ctx context.Context, spec string) (libtunnel.TunnelV1, context.CancelFunc) {
	log, token, local := r.log, r.token, r.local

	// Events: the tunnel's lifecycle, logged. Nothing is decided here any
	// more. A tunnel the edge has disowned is ended by libtunnel itself — its
	// edge watcher asks while the tunnel is short of connections, and the
	// edge's refusal cancels the tunnel with the reason — so Done fires, Err
	// carries it, and wait returns it. There used to
	// be a counter here folding gone verdicts into that decision, from when
	// libtunnel only reported and never acted.
	listen := func(e libtunnel.Event) {
		log.Debug("received event", "e", e)
	}

	// What the cache has is what the mint is hinted with, and nothing when it
	// has nothing: From("") mints fresh, so there is one call and no branch.
	// No second attempt on failure either — From asks the edge about a hint
	// before minting, so a dead cached spec is already a fresh mint by the
	// time it could fail, and the one failure left is a provider that could
	// not be reached, which a remint could not reach either.
	//
	// The tunnel gets a context of its own under the run's: ending it ends
	// this tunnel and nothing else.
	tctx, stop := context.WithCancel(ctx)
	tun := r.tunnel(spec).
		WithToken(token).
		// Which tunneld is asking, ahead of the libtunnel comment the mint
		// adds after it. Always tunneld's, under whatever name an embedding
		// program mounts the command as: it is this code minting.
		WithHeader("User-Agent", r.userAgent).
		WithLogger(log).
		WithContext(tctx).
		WithEventListener(listen).
		// One address, whatever the run exposes: which origin a request
		// reaches is decided in front of them, by the router.
		WithLocalURL(local)
	return tun, stop
}

// up waits for tun to come up and puts it in front of everybody: the addresses
// on stdout, the map on stderr, the bound origins told where they answer from,
// the provider's messages learned, the browser or the console opened, the spec
// saved, a waiting launcher handed back, the stop hint. It answers with the
// spec it saved; a tunnel that never comes up is the error.
func (r *RunImpl) up(ctx context.Context, tun libtunnel.TunnelV1) (string, error) {
	cmd, log, origins, bound, spec := r.cmd, r.log, r.origins, r.bound, r.cache
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()

	// Something turning, because the wait below is the long one: minting,
	// dialing the edge, and the public URL answering from here, which is
	// seconds of a program that has printed its version and gone quiet.
	//
	// On stderr with the banner it follows, never stdout: that stream is one
	// public address per origin and nothing else, and a spinner in it is a
	// carriage return where a script expected a URL.
	//
	// Only when somebody is watching, and only when nothing else is writing
	// there. A log line lands on top of a spinner, so a run with its logger
	// on gets the lines instead — they say more than a spinner does, and they
	// are what the operator asked for.
	//
	// On Ready rather than URL, because URL is the value and a spinner wants
	// the channel: Ready hands out one per call, delivering once and closing,
	// so this waiter and the URL below both see the tunnel come up — or both
	// see it fail, since Ready closes when a tunnel ends as surely as it
	// delivers when one comes up.
	//
	// Not while a frame an earlier tunnel drew is up: the console is the
	// frame's, and a spinner there animates over it.
	if display.IsInteractive(cmd) && r.spinner && r.screen == nil {
		console.Loading(stderr, tun.Ready(), "Creating tunnel...")
	}

	// URL blocks until the public URL is verified to work from here — the
	// edge's route registered and answering, which is the moment an address
	// is worth handing to anybody — and is nil if the tunnel, or the context
	// this run gave it, ends first. Err is why.
	public := tun.URL()
	if public == nil {
		return "", cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)
	}

	// A bound container is served before the tunnel exists —
	// the binding is what the tunnel is handed to proxy to — so
	// this is the first moment anything down there can be told
	// where it answers from outside. Each origin gets its own
	// address rather than the bare one, because with several of
	// them it is the routing parameter that reaches this one.
	addresses := make([]string, origins.Len())
	for i := range addresses {
		addresses[i] = publicURL(public, i, origins.Len())
	}
	bound.Announce(addresses)

	// The panel's address when there is a panel, "" when there is
	// not: the browser answers the question, and everything below
	// reads the answer.
	view := r.display.URL(r.multiview, public, origins)

	// The report: write the human-readable map to stderr, a line
	// per public address with the origins it reaches indented
	// beneath it. With a panel that is one address and every
	// origin; without, one address per origin.
	//
	// Nothing goes to stdout. It used to carry one bare URL per
	// origin as a machine interface, which meant every address
	// printed twice wherever the two streams landed together — a
	// terminal, a container's logs — and the de-duplication that
	// hid it could only see the case where one file descriptor was
	// literally the other. Under Docker they are two pipes that
	// merge downstream, so it never fired where it was needed most.
	// The banner is already on stderr by the time this runs: it was
	// printed by the builder, before minting, so it survives a mint that
	// fails.
	//
	// Nothing waits here for the address to answer. Ready delivered
	// on the public URL verified from this machine, so every line
	// below names an address a script can read and use in the same
	// breath.
	// Every public address gets a line, with what it reaches
	// indented beneath. A panel is the case where one address
	// reaches them all; otherwise each origin has an address of its
	// own. One shape either way, and no column to keep aligned as
	// hostnames change length.
	//
	// Not while a frame an earlier tunnel drew is up. A frame is drawn only
	// on a console that is both streams, so no script is reading these; the
	// lines would land on the frame, and the frame has the new addresses
	// already — Announce told it, above.
	if r.screen == nil {
		if view != "" {
			fmt.Fprintf(stdout, "%s\n", view)
			for _, origin := range origins.URLs() {
				fmt.Fprintf(stderr, "  -> %s\n", label(origin))
			}
		} else {
			for i, origin := range origins.URLs() {
				fmt.Fprintf(stdout, "%s\n", publicURL(public, i, origins.Len()))
				fmt.Fprintf(stderr, "  -> %s\n", label(origin))
			}
		}
	}

	// An origin nothing is listening on is the one thing here the person who
	// ran this can fix; a visitor already gets a page saying so. Logged for
	// now, after the addresses so a slow dial holds back nothing a script is
	// waiting for.
	//
	// TODO(#209): show it to the operator beneath the map, from the builder,
	// once there is one place that prints the run's human lines.
	for _, i := range r.router.Unanswered(ctx, origins) {
		log.Warn("nothing is listening on an origin yet", "origin", origins.At(i).Redacted())
	}

	// What the provider said with the spec, learned here because Messages
	// resolves the spec, which URL returning has already done. Every run,
	// cached or fresh — the messages ride the envelope with the spec they
	// came with. Nothing is printed for it: stderr is the map and the logs,
	// and the frame and the panel are where the provider's word is read.
	r.motd.Learn(tun.Messages(), log)

	// Putting the tunnel in front of a person is the browser package's, both
	// ways it can be done: a tab, or the console this was started from. What
	// is reported here is only what it cannot see for itself.
	//
	// Whether there is a console at all is the console package's answer, not
	// this function's: it is handed the bound origins and this command's own
	// streams and says nil when there is nothing to draw or nowhere to draw
	// it. One page, never a fan of tabs — the panel when there is one, since
	// it reaches every origin, and otherwise the default origin itself.
	//
	// Reported after the addresses, so what a person came for is on the
	// screen before a frame takes it, and left behind when that frame ends: a
	// detach gives the console back and the tunnel goes on without it.
	// OPEN=false is the hammer, and it is spelled as the two facts Open
	// already takes rather than as a gate around the call: there is no console
	// to draw on, and the caller has decided against a tab. How a run is shown
	// stays one switch in display, and the hint below still fires — from here
	// this is a run with no screen, which is exactly what it is.
	//
	// A frame an earlier tunnel drew is still up and showing the same origins,
	// so it is kept: nothing is opened again, and no hint is printed under it.
	showing := r.screen != nil
	screen, open := r.screen, r.open
	if !showing {
		screen = r.console.For(bound, cmd)
	}
	if os.Getenv(openEnv) == "false" {
		shown := false
		screen, open = nil, &shown
	}
	addr := cmp.Or(view, publicURL(public, 0, origins.Len()))

	// The address as a code for a phone, when asked for: the one address Open
	// is handed below, so one code however many origins there are. On stderr
	// with the map it follows, since stdout is the addresses and nothing else,
	// and before a detached run's streams move to its log, so the caller a
	// launcher is holding the prompt for gets it. Not when a frame is about to
	// take the console: Ctrl+K q is the code there, sized to the pane, and one
	// printed now would only sit behind it. The code itself is the display's,
	// which draws it; printing it is the run's, which knows when.
	//
	// And no tab: a run that asked for a code is being opened on a phone, and
	// a tab on this machine would be a second copy nobody asked for. A caller
	// who decided with WithOpen still wins.
	if r.qr && screen == nil {
		if lines, err := r.display.QR(addr); err != nil {
			log.Warn("the address could not be drawn as a QR code", "error", err)
		} else {
			fmt.Fprintln(stderr, strings.Join(lines, "\n"))
		}
	}
	if r.qr && open == nil {
		shown := false
		open = &shown
	}

	if !showing {
		r.display.Open(ctx, log,
			display.WithAddr(addr),
			display.WithForced(open),
			display.WithStderr(stderr),
			display.WithInteractive(display.IsInteractive(cmd)),
			display.WithScreen(screen),
		)
	}
	r.screen = screen
	// After the URL is live, so what gets cached is a tunnel that
	// came up rather than one that was merely asked for.
	// The hostname beside the rest of what the run settled on: never read
	// back, but the one line somebody opening the file wants first.
	tracking := maps.Clone(r.tracking)
	if tracking == nil {
		tracking = map[string]string{}
	}
	tracking[ltv1.HostnameEnv] = public.Hostname()
	saved := tun.Serialize()
	spec.Save(
		cache.WithOrigins(origins),
		cache.WithSpec(saved),
		cache.WithTracking(tracking),
		cache.WithSecret(tun.Secret()),
		cache.WithLog(log),
	)

	// A launcher waiting to hand the console back gets it now, after the
	// save, so the file it reads to say what is running is there. Ctrl-C
	// there will not reach this run, so it gets no hint saying so.
	// Its stdout and stderr go to the run's log file, so what bypasses the
	// logger — a panic's trace — is still somewhere; the logger itself stops
	// showing lines there, since the file already keeps them.
	detached := r.pid != nil && r.pid.Detach(r.logs.File(), log)
	if detached {
		r.logs.Detach()
	}
	if screen == nil && !detached {
		// Nothing is going to be drawn here. The addresses are up, the run
		// blocks from now on, and the signal is the only thing left on this
		// side of it.
		fmt.Fprintln(stderr, r.hint)
	}

	return saved, nil
}

// wait blocks until the run is over or a new spec arrives for it. A new spec
// is next, with respec true. Otherwise it says how the run ended: nil for a
// signal or a viewer asking it to end, and the tunnel's own verdict when it
// ends first.
//
// saved is the spec up saved for tun. The cache hands on every spec it takes,
// that one included, and it is this tunnel already: it is passed over. The
// save lands it once, ahead of anything a caller sends, so the first is the
// save's own and passes quietly; one sent again is somebody asking for the
// tunnel the run already has, and the log says it was kept.
func (r *RunImpl) wait(ctx context.Context, tun libtunnel.TunnelV1, saved string) (next string, respec bool, err error) {
	bound, log := r.bound, r.log
	var specs <-chan string
	if r.cache != nil {
		specs = r.cache.Spec()
	}

	// A viewer asking to end the run is the third way this stops, beside a
	// signal and the tunnel failing. Nothing is wrong when it happens, so it
	// reads as a clean exit — the builder's deferred teardown takes the origins,
	// the programs they started and the tunnel with it.
	own := true
	for waiting := true; waiting; {
		select {
		case spec := <-specs:
			if !sameSpec(spec, saved) {
				log.Info("a new spec arrived for the run")
				return spec, true, nil
			}
			if !own {
				log.Info("the spec sent is the one this tunnel already has; keeping the tunnel")
			}
			own = false
		case <-ctx.Done():
			waiting = false
		case <-tun.Done():
			waiting = false
		case <-bound.Done():
			log.Info("a viewer asked this run to end; stopping")
			return "", false, nil
		}
	}

	// A signal cancels ctx, and cancelling ctx ends the tunnel too, so both
	// arms above are ready at once and the race would otherwise decide what
	// an operator is shown. The signal is checked first: a tunnel that came
	// up and was then told to stop is a clean exit, whatever the teardown
	// has to say for itself. Anything else is the tunnel's own verdict —
	// the edge disowning it arrives here as Cancel's cause.
	if ctx.Err() != nil {
		return "", false, nil
	}
	return "", false, tun.Err()
}

// sameSpec reports whether a and b are one spec: equal as JSON — the same
// envelope, whatever its spacing or the order of its keys, numbers compared
// as written rather than as floats — or, when either is not JSON, equal as
// strings. A spec sent back as it was saved, reformatted on the way, is still
// the tunnel the run already has.
func sameSpec(a, b string) bool {
	x, errA := decodeSpec(a)
	y, errB := decodeSpec(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return reflect.DeepEqual(x, y)
}

// decodeSpec is a spec as JSON, with its numbers kept as they were written. A
// trailing value after the first is not one spec, and is an error.
func decodeSpec(spec string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(spec))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	return v, nil
}

// openEnv is the hammer, and the one thing about how a run is shown that is
// typed rather than derived: OPEN=false shows this run nothing. No frame takes
// the console and no tab is launched, so the log lines keep the stderr they
// would otherwise have been muted for — which is the whole of why it exists,
// since the frame is drawn over exactly the output somebody reaching for it is
// trying to read.
//
// No flag, no TUNNELD_ mirror, and no row in the README. Every other variable
// this reads is one half of a knob an operator is meant to find; this is a way
// out of the console for the case the console is in the way, and a knob that
// turns the product off is not a feature of it. The bare word is the cost of
// being quick to type, and only the exact value "false" swings it: an OPEN
// that some other program left in the environment is overwhelmingly unlikely
// to be spelled that way, and anything this does not recognise leaves the
// derived decision alone.
const openEnv = "OPEN"

// publicURL is the address origin i answers on, out of n origins: the tunnel's
// URL with a bare ?i routing parameter. Bare is load-bearing — a valued
// parameter ("?1=x") is application data the proxy forwards, while the bare
// form is the routing directive it consumes and strips before the request
// reaches the origin.
//
// The default origin is explicit too, as ?0, whenever there is more than one.
// A bare URL routes by the referring page and then by the sticky cookie, so
// once a browser has visited ?1 a plain address no longer reaches origin 0 —
// only an explicit index clears a previous choice. An address that stops
// working after someone clicks around is worse than a longer one.
//
// A lone origin has nothing to route between, so n of 1 gives the plain URL
// and no parameter at all.
func publicURL(public *url.URL, i, n int) string {
	if n <= 1 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}

// label is an origin as a person reads it: a program without its arguments.
//
// The arguments ride the origin as a query because that is how they travel —
// argv, the environment and a seed all spell them the same way — but a query
// is a carrier, not a label. What the map says a tunnel reaches is the
// program; how it was started is in the cache file beside the spec, and in
// the key that names the file. An http origin's query is part of its address
// and stays.
func label(u *url.URL) string {
	if u.Scheme == v1.ExecScheme && u.RawQuery != "" {
		bare := *u
		bare.RawQuery = ""
		return bare.String()
	}
	return u.String()
}
