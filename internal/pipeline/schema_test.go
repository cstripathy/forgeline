package pipeline

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

var update = flag.Bool("update", false, "regenerate the committed JSON Schema")

func reflectField(v any, name string) (reflect.StructField, bool) {
	return reflect.TypeOf(v).FieldByName(name)
}

func schemaPath() string {
	// Tests run in the package directory: internal/pipeline.
	return filepath.Join("..", "..", filepath.FromSlash(SchemaFile))
}

func TestSchemaMatchesCommittedFile(t *testing.T) {
	got, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error: %v", err)
	}
	path := schemaPath()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading committed schema: %v (run: go test ./internal/pipeline -update)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("committed schema is out of date; run: go test ./internal/pipeline -update")
	}
}

func TestSchemaIsDeterministic(t *testing.T) {
	first, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		next, err := Schema()
		if err != nil {
			t.Fatal(err)
		}
		if string(next) != string(first) {
			t.Fatal("Schema() output differs between calls")
		}
	}
}

func TestSchemaStructure(t *testing.T) {
	raw, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	if s["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("unexpected $schema: %v", s["$schema"])
	}
	if s["additionalProperties"] != false {
		t.Errorf("root must reject unknown properties")
	}
	assertStrings(t, "root required", s["required"], []string{"version", "name", "steps"})

	props := s["properties"].(map[string]any)
	version := props["version"].(map[string]any)
	assertStrings(t, "version enum", version["enum"], []string{VersionV1Alpha1})

	steps := props["steps"].(map[string]any)
	if steps["minItems"] != float64(1) {
		t.Errorf("steps minItems = %v, want 1", steps["minItems"])
	}
	if ref := steps["items"].(map[string]any)["$ref"]; ref != "#/$defs/Step" {
		t.Errorf("steps items $ref = %v", ref)
	}

	step := s["$defs"].(map[string]any)["Step"].(map[string]any)
	if step["additionalProperties"] != false {
		t.Errorf("Step must reject unknown properties")
	}
	assertStrings(t, "Step required", step["required"], []string{"id", "image"})

	oneOf, ok := step["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("Step oneOf = %v, want two alternatives (run, command)", step["oneOf"])
	}

	stepProps := step["properties"].(map[string]any)
	id := stepProps["id"].(map[string]any)
	if id["pattern"] != idPattern {
		t.Errorf("Step id pattern = %v, want %s", id["pattern"], idPattern)
	}
	needs := stepProps["needs"].(map[string]any)
	if needs["items"].(map[string]any)["pattern"] != idPattern {
		t.Errorf("Step needs items pattern missing")
	}
	env := stepProps["env"].(map[string]any)
	if env["additionalProperties"].(map[string]any)["type"] != "string" {
		t.Errorf("Step env values must be strings")
	}
}

func assertStrings(t *testing.T, what string, got any, want []string) {
	t.Helper()
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("%s: got %T, want a list", what, got)
	}
	if len(list) != len(want) {
		t.Fatalf("%s = %v, want %v", what, list, want)
	}
	for i, w := range want {
		if list[i] != w {
			t.Fatalf("%s = %v, want %v", what, list, want)
		}
	}
}
