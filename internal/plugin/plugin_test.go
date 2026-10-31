package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const digest = "example/echo@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// The test binary doubles as a fake `docker`: it records how it was called, then acts as an MCP
// server (or misbehaves) depending on FAKE_MODE.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_DOCKER") == "1" {
		fakeDocker()
		return
	}
	os.Exit(m.Run())
}

func fakeDocker() {
	log := os.Getenv("FAKE_LOG")
	if len(os.Args) > 1 && os.Args[1] == "rm" {
		f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		fmt.Fprintln(f, "RM "+strings.Join(os.Args[2:], " "))
		f.Close()
		return
	}
	rec, _ := json.Marshal(map[string]any{"argv": os.Args[1:], "secret": os.Getenv("API_KEY"), "leak": os.Getenv("FORGELINE_TOKEN")})
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, string(rec))
	f.Close()
	mode := os.Getenv("FAKE_MODE")
	if mode == "hang" {
		time.Sleep(time.Minute)
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &m)
		if m.ID == nil {
			continue
		}
		switch m.Method {
		case "initialize":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"serverInfo":{"name":"fake"}}}`+"\n", *m.ID)
		case "tools/list":
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"echo","description":"d","inputSchema":{}}]}}`+"\n", *m.ID)
		case "tools/call":
			text := "hello"
			if mode == "big" {
				text = strings.Repeat("x", 100000)
			}
			fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}],"isError":%v}}`+"\n", *m.ID, text, mode == "toolerror")
		}
	}
}

func runner(t *testing.T, mode string) (*Runner, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "log")
	t.Setenv("FORGELINE_TOKEN", "server-secret")
	return &Runner{Docker: os.Args[0], Limits: Limits{Timeout: 3 * time.Second, MaxOutput: 1000},
		ExtraEnv: []string{"FAKE_DOCKER=1", "FAKE_LOG=" + log, "FAKE_MODE=" + mode}}, log
}

func argvOf(t *testing.T, log string) (argv []string, secret, leak string) {
	t.Helper()
	b, _ := os.ReadFile(log)
	line := strings.Split(strings.TrimSpace(string(b)), "\n")[0]
	var rec struct {
		Argv         []string
		Secret, Leak string
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("log %q: %v", b, err)
	}
	return rec.Argv, rec.Secret, rec.Leak
}

func has(argv []string, seq ...string) bool {
	for i := range argv {
		if i+len(seq) <= len(argv) && strings.Join(argv[i:i+len(seq)], "\x00") == strings.Join(seq, "\x00") {
			return true
		}
	}
	return false
}

func TestSpecCheck(t *testing.T) {
	good := Spec{ID: "echo", Image: digest}
	if err := good.Check(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Spec{
		"bare name":      {ID: "echo", Image: "example/echo"},
		"tag only":       {ID: "echo", Image: "example/echo:latest"},
		"short digest":   {ID: "echo", Image: "example/echo@sha256:abc"},
		"flag injection": {ID: "echo", Image: "--privileged@sha256:" + strings.Repeat("0", 64)},
		"bad id":         {ID: "../x", Image: digest},
		"bad env":        {ID: "echo", Image: digest, Env: map[string]string{"a-b": "v"}},
	} {
		if s.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestArgvIsLockedDownAndKeepsSecretsOffTheCommandLine(t *testing.T) {
	r, log := runner(t, "")
	_, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest, Env: map[string]string{"API_KEY": "s3cret"}}, "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	argv, secret, leak := argvOf(t, log)
	for _, want := range [][]string{{"--read-only"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}, {"--network", "none"}, {"--user", "65534:65534"},
		{"--pids-limit", "64"}, {"--memory", "256m"}, {"--pull", "never"}, {"-e", "API_KEY"}, {"--rm"}} {
		if !has(argv, want...) {
			t.Errorf("argv lacks %v: %v", want, argv)
		}
	}
	if argv[len(argv)-1] != digest {
		t.Errorf("image must be last: %v", argv)
	}
	if strings.Contains(strings.Join(argv, " "), "s3cret") {
		t.Error("a secret value is on the command line")
	}
	for _, bad := range []string{"--privileged", "-v", "--volume", "--mount", "docker.sock", "--network host"} {
		if strings.Contains(strings.Join(argv, " "), bad) {
			t.Errorf("argv contains %s", bad)
		}
	}
	if secret != "s3cret" {
		t.Errorf("the granted secret did not reach the docker client: %q", secret)
	}
	if leak != "" {
		t.Error("the server's own environment leaked to the plugin's docker client")
	}
}

func TestNetworkOnlyWhenGranted(t *testing.T) {
	r, log := runner(t, "")
	if _, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest, Network: true}, "echo", nil); err != nil {
		t.Fatal(err)
	}
	if argv, _, _ := argvOf(t, log); !has(argv, "--network", "bridge") || has(argv, "--network", "none") {
		t.Errorf("argv = %v", argv)
	}
}

func TestCallListOutputCapAndToolErrors(t *testing.T) {
	r, _ := runner(t, "")
	text, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest}, "echo", map[string]any{"x": 1})
	if err != nil || text != "hello" {
		t.Fatalf("got %q, %v", text, err)
	}
	tools, err := r.Tools(context.Background(), Spec{ID: "echo", Image: digest})
	if err != nil || len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %v, %v", tools, err)
	}
	r, _ = runner(t, "big")
	text, _ = r.Call(context.Background(), Spec{ID: "echo", Image: digest}, "echo", nil)
	if len(text) > 1100 || !strings.HasSuffix(text, "[output cut]") {
		t.Errorf("output not capped: %d bytes", len(text))
	}
	r, _ = runner(t, "toolerror")
	if _, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest}, "echo", nil); err == nil {
		t.Error("a tool error must be an error")
	}
}

func TestTimeoutRemovesTheContainer(t *testing.T) {
	r, log := runner(t, "hang")
	r.Limits.Timeout = 700 * time.Millisecond
	start := time.Now()
	_, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest}, "echo", nil)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "RM -f forgeline-plugin-") {
		t.Errorf("container not removed after a timeout: %q", b)
	}
}

func TestCallsToOnePluginRunOneAtATime(t *testing.T) {
	r, _ := runner(t, "")
	// Two calls in parallel must both succeed; the lock keeps them from overlapping.
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := r.Call(context.Background(), Spec{ID: "echo", Image: digest}, "echo", nil)
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
