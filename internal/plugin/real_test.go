package plugin

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealDocker runs the sample plugin in a real container. Build it with
// scripts/dev/sample-plugin/build.sh and set FORGELINE_SAMPLE_PLUGIN_IMAGE to the id it prints.
func TestRealDocker(t *testing.T) {
	image := os.Getenv("FORGELINE_SAMPLE_PLUGIN_IMAGE")
	if image == "" {
		t.Skip("set FORGELINE_SAMPLE_PLUGIN_IMAGE to run against a real Docker")
	}
	r := &Runner{Docker: "docker", Limits: Limits{Timeout: 60 * time.Second}}
	ctx := context.Background()
	if err := r.Ensure(ctx, image); err != nil {
		t.Fatal(err)
	}
	spec := Spec{ID: "sample", Image: image, Env: map[string]string{"SAMPLE_KEY": "k"}}
	tools, err := r.Tools(ctx, spec)
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	if out, err := r.Call(ctx, spec, "shout", map[string]any{"text": "hello"}); err != nil || out != "HELLO" {
		t.Fatalf("shout = %q, %v", out, err)
	}
	out, err := r.Call(ctx, spec, "probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"secret=true", "network=false", "writable_root=false", "uid=65534"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe = %q, want %s", out, want)
		}
	}
	// Granting network changes only that.
	spec.Network = true
	if out, _ := r.Call(ctx, spec, "probe", nil); !strings.Contains(out, "writable_root=false") {
		t.Errorf("probe with network = %q", out)
	}
}
