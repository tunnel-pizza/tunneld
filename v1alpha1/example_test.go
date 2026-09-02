// The godoc examples for the tunneld builder. `example_test.go` is an
// established Go idiom that renders as documentation on pkg.go.dev, so it
// keeps its name rather than folding into v1alpha1_test.go — the one
// exception, with e2e, to CONTRIBUTING's one-test-file-per-source rule.
package v1alpha1_test

import (
	"fmt"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// New returns an unconfigured Builder. Configure it with the With* methods and
// finalize with Build, which yields a *cobra.Command ready to Execute.
func ExampleNew() {
	cmd := v1alpha1.New().WithURL("http://localhost:3000").Build()

	fmt.Println(cmd.Name())
	// Output: tunneld
}

// Several --url values share one public hostname: the first is the default
// origin and each later one answers on a bare ?n parameter.
func ExampleNew_multipleOrigins() {
	cmd := v1alpha1.New().
		WithURL("http://localhost:3000", "http://localhost:4000").
		Build()

	fmt.Println(cmd.Flags().Lookup("url").DefValue)
	// Output: [http://localhost:3000,http://localhost:4000]
}

// WithName mounts tunneld under another program's verb, so an embedding CLI
// documents it as its own subcommand.
func ExampleNew_embedded() {
	cmd := v1alpha1.New().WithName("expose").WithURL("http://localhost:3000").Build()

	fmt.Println(cmd.Name())
	// Output: expose
}
