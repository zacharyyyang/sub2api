package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// MCP stdio uses newline-delimited JSON-RPC. Stdout is reserved for protocol frames.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func main() {
	declarations := flag.String("tools-json", "[]", "request-scoped MCP tool declarations")
	flag.Parse()
	var tools []service.WBBridgeTool
	if err := json.Unmarshal([]byte(*declarations), &tools); err != nil {
		fmt.Fprintln(os.Stderr, "invalid MCP tool declarations")
		os.Exit(1)
	}
	if err := service.ValidateWBBridgeTools(tools); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := serve(os.Stdin, os.Stdout, tools); err != nil {
		fmt.Fprintln(os.Stderr, "MCP stdio transport failed")
		os.Exit(1)
	}
}

func serve(in io.Reader, out io.Writer, tools []service.WBBridgeTool) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "Parse error"}}); err != nil {
				return err
			}
			continue
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			id := req.ID
			if len(id) == 0 {
				id = json.RawMessage("null")
			}
			if err := encoder.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{-32600, "Invalid Request"}}); err != nil {
				return err
			}
			continue
		}
		// Notifications have no response, including notifications/initialized.
		if len(req.ID) == 0 {
			continue
		}
		response := dispatch(req, tools)
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func dispatch(req rpcRequest, tools []service.WBBridgeTool) rpcResponse {
	response := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
			response.Error = &rpcError{-32602, "Invalid initialize parameters"}
			break
		}
		// This server uses only the stable tools subset of the MCP protocol.
		version := params.ProtocolVersion
		switch version {
		case "2024-11-05", "2025-03-26", "2025-06-18":
		default:
			version = "2024-11-05"
		}
		response.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "wb-mcp-bridge", "version": "1.0.0"},
		}
	case "ping":
		response.Result = map[string]any{}
	case "tools/list":
		if tools == nil {
			tools = []service.WBBridgeTool{}
		}
		response.Result = map[string]any{"tools": tools}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
			response.Error = &rpcError{-32602, "Invalid tool call parameters"}
			break
		}
		text, err := service.ExecuteWBBridgeTool(tools, params.Name, params.Arguments)
		isError := err != nil
		if err != nil {
			text = err.Error()
		}
		response.Result = map[string]any{
			"content": []any{map[string]string{"type": "text", "text": text}},
			"isError": isError,
		}
	default:
		response.Error = &rpcError{-32601, "Method not found"}
	}
	return response
}
