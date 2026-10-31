package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestRegenerateMarketHashes rewrites the sha256 in each manifest after an intended edit:
// FORGELINE_UPDATE_MARKET=1 go test ./internal/server -run RegenerateMarketHashes
func TestRegenerateMarketHashes(t *testing.T) {
	if os.Getenv("FORGELINE_UPDATE_MARKET") == "" {
		t.Skip("set FORGELINE_UPDATE_MARKET=1 to rewrite the manifests")
	}
	dir := "market"
	if d := os.Getenv("FORGELINE_UPDATE_MARKET_DIR"); d != "" {
		dir = d // a local source (FORGELINE_MARKET_DIR) instead of the built-in one
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, _ := os.ReadFile(dir + "/" + f.Name())
		var p MarketPackage
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		p.SHA256 = p.contentHash()
		out, _ := json.MarshalIndent(p, "", "  ")
		if err := os.WriteFile(dir+"/"+f.Name(), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
