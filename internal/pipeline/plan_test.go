package pipeline

import (
	"errors"
	"reflect"
	"testing"
)

func steps(spec ...[]string) []Step {
	out := make([]Step, len(spec))
	for i, s := range spec {
		out[i] = Step{ID: s[0], Image: "alpine", Run: "true", Needs: s[1:]}
	}
	return out
}

func layerIDs(g *Graph) [][]string {
	out := make([][]string, len(g.Layers))
	for i, l := range g.Layers {
		for _, s := range l {
			out[i] = append(out[i], s.ID)
		}
	}
	return out
}

func TestPlanLayers(t *testing.T) {
	tests := []struct {
		name  string
		steps []Step
		want  [][]string
	}{
		{"linear chain", steps([]string{"a"}, []string{"b", "a"}, []string{"c", "b"}), [][]string{{"a"}, {"b"}, {"c"}}},
		{"diamond", steps([]string{"a"}, []string{"b", "a"}, []string{"c", "a"}, []string{"d", "b", "c"}),
			[][]string{{"a"}, {"b", "c"}, {"d"}}},
		{"independent", steps([]string{"x"}, []string{"y"}, []string{"z"}), [][]string{{"x", "y", "z"}}},
		{"forward reference", steps([]string{"b", "a"}, []string{"a"}), [][]string{{"a"}, {"b"}}},
		{"uneven depth", steps([]string{"a"}, []string{"b", "a"}, []string{"c"}, []string{"d", "b", "c"}),
			[][]string{{"a", "c"}, {"b"}, {"d"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, err := Plan(&Pipeline{Version: VersionV1Alpha1, Name: "p", Steps: tt.steps})
			if err != nil {
				t.Fatalf("Plan() = %v", err)
			}
			if got := layerIDs(g); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("layers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanDependents(t *testing.T) {
	g, err := Plan(&Pipeline{Version: VersionV1Alpha1, Name: "p",
		Steps: steps([]string{"a"}, []string{"b", "a"}, []string{"c", "a"})})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c"}; !reflect.DeepEqual(g.Dependents["a"], want) {
		t.Fatalf("Dependents[a] = %v, want %v", g.Dependents["a"], want)
	}
}

func TestPlanCycle(t *testing.T) {
	p := &Pipeline{Version: VersionV1Alpha1, Name: "p",
		Steps: steps([]string{"a", "c"}, []string{"b", "a"}, []string{"c", "b"})}
	_, err := Plan(p)
	if err == nil || err.Error() != "cycle: a -> c -> b -> a" {
		t.Fatalf("Plan() error = %v, want cycle: a -> c -> b -> a", err)
	}
}

func TestPlanCycleNotAtStart(t *testing.T) {
	p := &Pipeline{Version: VersionV1Alpha1, Name: "p",
		Steps: steps([]string{"x", "a"}, []string{"a", "b"}, []string{"b", "a"})}
	_, err := Plan(p)
	if err == nil || err.Error() != "cycle: a -> b -> a" {
		t.Fatalf("Plan() error = %v, want cycle: a -> b -> a", err)
	}
}

func TestPlanSelfCycleCaughtByValidate(t *testing.T) {
	p := &Pipeline{Version: VersionV1Alpha1, Name: "p", Steps: steps([]string{"a", "a"})}
	_, err := Plan(p)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Plan() error = %v, want *ValidationError", err)
	}
}
