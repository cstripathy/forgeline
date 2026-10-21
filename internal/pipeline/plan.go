package pipeline

import (
	"fmt"
	"strings"
)

// Graph is a pipeline's dependency graph, ready for a scheduler.
type Graph struct {
	// Layers groups steps so every step's dependencies are in earlier layers. Steps in one
	// layer are independent and can run in parallel. Order inside a layer is declaration order.
	Layers [][]*Step
	// Dependents maps a step ID to the IDs of steps that need it, in declaration order.
	Dependents map[string][]string
}

// Plan validates the pipeline and builds its dependency graph. It returns Validate's
// *ValidationError unchanged, or an error naming the path of a dependency cycle, for
// example "cycle: a -> b -> c -> a".
func Plan(p *Pipeline) (*Graph, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	byID := make(map[string]*Step, len(p.Steps))
	for i := range p.Steps {
		byID[p.Steps[i].ID] = &p.Steps[i]
	}

	// Depth-first walk in declaration order, so cycle reports are deterministic. depth[id]
	// is the layer index: 0 for no dependencies, else 1 + the deepest dependency.
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(p.Steps))
	depth := make(map[string]int, len(p.Steps))
	var stack []string

	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case done:
			return nil
		case visiting:
			start := 0
			for i, s := range stack {
				if s == id {
					start = i
					break
				}
			}
			path := append(append([]string{}, stack[start:]...), id)
			return fmt.Errorf("cycle: %s", strings.Join(path, " -> "))
		}
		state[id] = visiting
		stack = append(stack, id)
		d := 0
		for _, need := range byID[id].Needs {
			if err := visit(need); err != nil {
				return err
			}
			if depth[need]+1 > d {
				d = depth[need] + 1
			}
		}
		stack = stack[:len(stack)-1]
		depth[id] = d
		state[id] = done
		return nil
	}
	for i := range p.Steps {
		if err := visit(p.Steps[i].ID); err != nil {
			return nil, err
		}
	}

	g := &Graph{Dependents: make(map[string][]string)}
	for i := range p.Steps {
		s := &p.Steps[i]
		for len(g.Layers) <= depth[s.ID] {
			g.Layers = append(g.Layers, nil)
		}
		g.Layers[depth[s.ID]] = append(g.Layers[depth[s.ID]], s)
		for _, need := range s.Needs {
			g.Dependents[need] = append(g.Dependents[need], s.ID)
		}
	}
	return g, nil
}
