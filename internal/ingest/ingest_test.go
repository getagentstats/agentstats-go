package ingest

import "testing"

func TestURL(t *testing.T) {
	t.Setenv("AGENTSTATS_URL", "")
	if got := URL("mcp"); got != "https://agentstats.io/api/ingest/mcp" {
		t.Fatalf("default: %s", got)
	}
	t.Setenv("AGENTSTATS_URL", "http://localhost:3000/")
	if got := URL("cli"); got != "http://localhost:3000/api/ingest/cli" {
		t.Fatalf("AGENTSTATS_URL: %s", got)
	}
}
