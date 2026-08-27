// Command basic is the smallest tunneld example: build a value through the
// generic builder and print the result.
package main

import (
	"fmt"

	"github.com/tunnel-pizza/tunneld"
)

func main() {
	res := tunneld.New[string]().
		WithName("greeting").
		WithValue("hello world").
		Build()

	fmt.Printf("%s: %s\n", res.Name, res.Value)
}
