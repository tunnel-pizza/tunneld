// Package e2e builds the tunneld binary and drives it, asserting the exit code
// and output of every path that resolves without a network — help, the version
// banner, and each way a bad invocation is refused. Run with:
// go test -count=1 ./e2e
//
// Nothing here mints a tunnel. Bringing one up needs the public internet and a
// live provider, which would make the lane flaky and slow; the binary's job up
// to that point — parse, validate, refuse or proceed — is exactly what these
// cases pin. The tunnel itself is covered by the underlying library's own live
// tier.
package e2e
