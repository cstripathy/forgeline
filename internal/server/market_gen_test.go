package server

import (
	"encoding/json"
	"os"
	"testing"
)

// TestRegenerateMarketHashes rewrites the sha256 in each manifest after an intended edit:
// FORGELINE_UPDATE_MARKET=1 go test ./internal/server -run RegenerateMarketHashes
func TestRegenerateMarketHashes(t *testing.T) {
	if os.Getenv("FORGELINE_UPDATE_MARKET") == "" {
		t.Skip("set FORGELINE_UPDATE_MARKET=1 to rewrite the manifests")
	}
	files, _ := marketFS.ReadDir("market")
	for _, f := range files {
		b, _ := marketFS.ReadFile("market/" + f.Name())
		var p MarketPackage
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		p.SHA256 = p.contentHash()
		out, _ := json.MarshalIndent(p, "", "  ")
		if err := os.WriteFile("market/"+f.Name(), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
