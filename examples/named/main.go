// Command named shows the builder with a typed payload: a struct value carried
// through WithValue and rendered from the Result.
package main

import (
	"fmt"

	"github.com/tunnel-pizza/tunneld"
)

type widget struct {
	ID   int
	Tags []string
}

func main() {
	res := tunneld.New[widget]().
		WithName("widget").
		WithValue(widget{ID: 7, Tags: []string{"alpha", "beta"}}).
		Build()

	fmt.Printf("%s: %+v\n", res.Name, res.Value)
}
