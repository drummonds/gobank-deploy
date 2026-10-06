package flows

import wf "git.bytestone.uk/hum3/gobank-workflow"

// Definitions are the workflows this program runs, in the order the page
// and the diagrams show them. The engine's own registry (wf.Definitions)
// also holds the kinds it was built for and this program never runs.
var Definitions = []wf.Definition{DemoDefinition, DrillDefinition, PerfDefinition}

// DefinitionOf is the definition of t among Definitions; nil when t is
// not one this program runs.
func DefinitionOf(t wf.WorkflowType) *wf.Definition {
	for i := range Definitions {
		if Definitions[i].Type == t {
			return &Definitions[i]
		}
	}
	return nil
}
