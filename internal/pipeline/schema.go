package pipeline

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// SchemaFile is where the generated schema is committed, relative to the repository root.
const SchemaFile = "schema/pipeline.v1alpha1.schema.json"

// Schema generates the JSON Schema (draft 2020-12) for Pipeline from the Go types.
//
// Struct tags drive the output:
//
//	json:"name,omitempty"  property name; omitempty makes it optional
//	doc:"..."              description
//	pattern, enum          string constraints (enum is comma-separated)
//	minLength, minItems    size constraints
//	itemPattern            pattern applied to each element of an array
//
// The output is deterministic so it can be committed and checked for drift.
func Schema() ([]byte, error) {
	g := &generator{defs: map[string]map[string]any{}}
	root, err := g.object(reflect.TypeOf(Pipeline{}))
	if err != nil {
		return nil, err
	}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["title"] = "Forgeline pipeline"
	root["description"] = "A Forgeline pipeline, spec version " + VersionV1Alpha1 + "."

	// A rule that struct tags cannot express: a step sets exactly one of run or command.
	if step, ok := g.defs["Step"]; ok {
		step["oneOf"] = []any{
			map[string]any{"required": []string{"run"}},
			map[string]any{"required": []string{"command"}},
		}
	}
	if len(g.defs) > 0 {
		root["$defs"] = g.defs
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type generator struct {
	defs map[string]map[string]any
}

func (g *generator) object(t reflect.Type) (map[string]any, error) {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, optional := jsonName(f)
		if name == "" {
			continue
		}
		s, err := g.typ(f.Type)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		if err := applyTags(s, f); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}
		props[name] = s
		if !optional {
			required = append(required, name)
		}
	}
	out := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out, nil
}

func (g *generator) typ(t reflect.Type) (map[string]any, error) {
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Slice:
		items, err := g.typ(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings, got %s", t.Key())
		}
		values, err := g.typ(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": values}, nil
	case reflect.Struct:
		name := t.Name()
		if _, done := g.defs[name]; !done {
			g.defs[name] = nil // reserve the name so recursive types terminate
			def, err := g.object(t)
			if err != nil {
				return nil, err
			}
			g.defs[name] = def
		}
		return map[string]any{"$ref": "#/$defs/" + name}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %s", t.Kind())
	}
}

// jsonName returns the property name from the json tag and whether it is optional.
// A field tagged json:"-" or without a json tag is skipped (empty name).
func jsonName(f reflect.StructField) (name string, optional bool) {
	tag, ok := f.Tag.Lookup("json")
	if !ok || tag == "-" {
		return "", false
	}
	parts := strings.Split(tag, ",")
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			optional = true
		}
	}
	return parts[0], optional
}

func applyTags(s map[string]any, f reflect.StructField) error {
	if v, ok := f.Tag.Lookup("doc"); ok {
		s["description"] = v
	}
	if v, ok := f.Tag.Lookup("pattern"); ok {
		s["pattern"] = v
	}
	if v, ok := f.Tag.Lookup("enum"); ok {
		s["enum"] = strings.Split(v, ",")
	}
	if v, ok := f.Tag.Lookup("minLength"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("bad minLength %q", v)
		}
		s["minLength"] = n
	}
	if v, ok := f.Tag.Lookup("minItems"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("bad minItems %q", v)
		}
		s["minItems"] = n
	}
	if v, ok := f.Tag.Lookup("itemPattern"); ok {
		items, ok := s["items"].(map[string]any)
		if !ok {
			return fmt.Errorf("itemPattern is only valid on array fields")
		}
		items["pattern"] = v
	}
	return nil
}
