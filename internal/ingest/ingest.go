// Package ingest says where the SDK sends events.
package ingest

import (
	"os"
	"strings"
)

// URL is AgentStats' ingest URL for one kind of data ("mcp", "api", "cli"). It isn't an option;
// AGENTSTATS_URL (undocumented) is for AgentStats' own development and tests.
func URL(kind string) string {
	base := os.Getenv("AGENTSTATS_URL")
	if base == "" {
		base = "https://agentstats.io"
	}
	return strings.TrimRight(base, "/") + "/api/ingest/" + kind
}
