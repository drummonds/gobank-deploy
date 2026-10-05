// flowd2 writes the d2 source of gobank-deploy's workflow definitions, as
// gobank-workflow's diagram package draws them, one <type>-workflow.d2 per
// definition in the current directory: task docs:d2 renders them.
package main

import (
	"fmt"
	"os"

	wf "git.bytestone.uk/hum3/gobank-workflow"
	"git.bytestone.uk/hum3/gobank-workflow/diagram"

	"git.bytestone.uk/hum3/gobank-deploy/internal/flows"
)

func main() {
	for _, d := range []wf.Definition{flows.DemoDefinition, flows.DrillDefinition, flows.PerfDefinition} {
		name := string(d.Type) + "-workflow.d2"
		if err := os.WriteFile(name, []byte(diagram.Definition(d)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(name)
	}
}
