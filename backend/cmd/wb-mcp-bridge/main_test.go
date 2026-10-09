package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestStdioEcho(t *testing.T) {
	tools := []service.WBBridgeTool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"Glob","arguments":{}}}
`
	var out bytes.Buffer
	if err := serve(strings.NewReader(input), &out, tools); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected four responses: %s", out.String())
	}
	for _, want := range []string{`"protocolVersion":"2024-11-05"`, `"name":"echo"`, `ECHO: hello`, `"isError":true`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
}
