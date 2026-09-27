// flowd2 prints the d2 source of gobank-deploy's workflow definitions, as
// gobank-workflow's diagram package draws them: task d2 renders it.
package main

import (
	"fmt"

	"git.bytestone.uk/hum3/gobank-workflow/diagram"

	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
)

func main() { fmt.Print(diagram.Definition(flows.DemoDefinition)) }
