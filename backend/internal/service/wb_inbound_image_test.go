package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const wbTestImageDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

// TestWBInboundImageContent：WB 入站图像块按 CLI 原生 input_image 形态落地。
// 图像只能活在会话节点里（stdin 是纯文本通道），所以含图像的请求必须走会话
// 合成；末条 user 带图时不得退化成 resumePrompt 纯文本，否则图片被静默丢弃。
func TestWBInboundImageContent(t *testing.T) {
	t.Run("image_url object shape on last user message becomes session node", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"what is this?"},` +
			`{"type":"image_url","image_url":{"url":"` + wbTestImageDataURL + `"}}]}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Len(t, req.Messages[0].Images, 1)
		require.Equal(t, wbTestImageDataURL, req.Messages[0].Images[0])
		require.NotNil(t, req.Messages[0].Content)
		require.Equal(t, "what is this?", *req.Messages[0].Content)

		// 含图 ⇒ 必须合成会话链，且末条 user 落成节点而非 resumePrompt。
		require.NotEmpty(t, req.resumeSessionID)
		require.NotEqual(t, "what is this?", req.resumePrompt)
		require.Equal(t, "Please answer my latest message.", req.resumePrompt)

		nodes := wbTestDecodeSessionNodes(t, req.sessionJSONL)
		userNode := wbTestFindUserNodeWithImages(t, nodes)
		parts := wbTestContentParts(t, userNode)
		require.Equal(t, "input_text", parts[0]["type"])
		require.Equal(t, "what is this?", parts[0]["text"])
		require.Equal(t, "input_image", parts[1]["type"])
		require.Equal(t, wbTestImageDataURL, parts[1]["image"])
	})

	t.Run("system prefix lands on the same first user node as the image", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"system","content":"be brief"},` +
			`{"role":"user","content":[{"type":"text","text":"what is this?"},` +
			`{"type":"image_url","image_url":{"url":"` + wbTestImageDataURL + `"}}]}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)

		nodes := wbTestDecodeSessionNodes(t, req.sessionJSONL)
		parts := wbTestContentParts(t, wbTestFindUserNodeWithImages(t, nodes))
		require.Equal(t, "input_text", parts[0]["type"])
		require.Equal(t, "be brief\n\nwhat is this?", parts[0]["text"])
		require.Equal(t, "input_image", parts[1]["type"])
	})

	t.Run("input_image block with image field is accepted", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"input_image","image":"` + wbTestImageDataURL + `"}]}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Equal(t, []string{wbTestImageDataURL}, req.Messages[0].Images)

		nodes := wbTestDecodeSessionNodes(t, req.sessionJSONL)
		parts := wbTestContentParts(t, wbTestFindUserNodeWithImages(t, nodes))
		// 无文本块时不写空 input_text，content 只留图像块。
		require.Len(t, parts, 1)
		require.Equal(t, "input_image", parts[0]["type"])
	})

	t.Run("image_url string shape is accepted", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"image_url","image_url":"` + wbTestImageDataURL + `"}]}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Equal(t, []string{wbTestImageDataURL}, req.Messages[0].Images)
	})

	t.Run("image part on non-user role is rejected", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"system","content":[{"type":"image_url","image_url":{"url":"` + wbTestImageDataURL + `"}}]}]}`)
		_, _, _, err := wbParseInbound(body)
		require.ErrorContains(t, err, "image content parts are only supported on user messages")
	})

	t.Run("image part without url is rejected", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"image_url"}]}]}`)
		_, _, _, err := wbParseInbound(body)
		require.ErrorContains(t, err, "missing image url")
	})

	// URL 值域：data URL 与 http(s) 接受，其余 scheme 与畸形 data URL 明确拒绝。
	// 拒绝的理由是错误必须在网关层可诊断——透传未知 scheme 只会让 CLI 在无法
	// 定位的位置失败。
	t.Run("image url value domain", func(t *testing.T) {
		accepted := []string{
			"data:image/png;base64,AA==",
			"data:image/jpeg;base64,BB==",
			"https://example.com/a.png",
			"http://example.com/b.jpg",
		}
		for _, u := range accepted {
			require.NoError(t, wbValidateImageURL(u), "expected accepted: %s", u)
		}

		rejected := map[string]string{
			"file:///tmp/a.png":       "unsupported image url scheme",
			"ftp://example.com/a.png": "unsupported image url scheme",
			"/local/path/a.png":       "unsupported image url scheme",
			"base64,AA==":             "unsupported image url scheme",
			"data:image/png;base64":   "missing ',' separator",
			"data:,AA==":              "empty media type",
			"data:;base64,AA==":       "empty media type",
			"data: ,AA==":             "empty media type",
			"https://":                "missing host after scheme",
			"http://   ":              "missing host after scheme",
		}
		for u, want := range rejected {
			require.ErrorContains(t, wbValidateImageURL(u), want, "expected rejected: %s", u)
		}
	})

	t.Run("unsupported scheme fails the inbound request", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"image_url","image_url":{"url":"file:///tmp/a.png"}}]}]}`)
		_, _, _, err := wbParseInbound(body)
		require.ErrorContains(t, err, "invalid image url")
		require.ErrorContains(t, err, "unsupported image url scheme")
	})

	t.Run("text-only request keeps stdin prompt path", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
		req, prompt, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Empty(t, req.resumeSessionID, "text-only requests must not synthesize a session")
		require.Equal(t, "User: hello", prompt)
	})

	// 历史带图、无工具历史、末条是 assistant：会话因图像而合成，但没有任何工具
	// 执行结果，stdin 不能发「基于上方的工具执行结果继续」（该句在此不成立）。
	t.Run("image in history with assistant tail uses image resume prompt", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"look"},` +
			`{"type":"image_url","image_url":{"url":"` + wbTestImageDataURL + `"}}]},` +
			`{"role":"assistant","content":"I see a chart."}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.NotEmpty(t, req.resumeSessionID)
		require.Equal(t, "Please answer my latest message.", req.resumePrompt,
			"no tool history means the tool-results sentence would point at nothing")

		nodes := wbTestDecodeSessionNodes(t, req.sessionJSONL)
		require.NotNil(t, wbTestFindUserNodeWithImages(t, nodes), "history image must stay in the session chain")
	})

	// 有工具历史时继续沿用工具结果句：此语境下该句才成立。
	t.Run("tool history keeps the tool-results resume prompt", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":"run it"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",` +
			`"function":{"name":"echo","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"done"}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Equal(t, "Please continue based on the tool execution results above.", req.resumePrompt)
	})

	// 文本与图像交叉排布：文本按序拼接、图像按序收集，节点内统一为「文本块在前、
	// 图像块在后」。这是有意归一，不是丢序：纯文本折叠通道本身只有一个文本位，
	// 交叉位置无法用文本块表达。
	t.Run("interleaved text and images are normalized to text-then-images", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":[{"type":"text","text":"first "},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},` +
			`{"type":"text","text":"second"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,BB=="}}]}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Equal(t, "first second", *req.Messages[0].Content)
		require.Equal(t, []string{"data:image/png;base64,AA==", "data:image/png;base64,BB=="}, req.Messages[0].Images)

		parts := wbTestContentParts(t, wbTestFindUserNodeWithImages(t, wbTestDecodeSessionNodes(t, req.sessionJSONL)))
		require.Len(t, parts, 3)
		require.Equal(t, "input_text", parts[0]["type"])
		require.Equal(t, "first second", parts[0]["text"])
		require.Equal(t, "input_image", parts[1]["type"])
		require.Equal(t, "data:image/png;base64,AA==", parts[1]["image"])
		require.Equal(t, "input_image", parts[2]["type"])
		require.Equal(t, "data:image/png;base64,BB==", parts[2]["image"])
	})

	// 历史带图 + 无工具历史 + 末条空白 user：空白不能充当 prompt，兜底必须回到
	// 同一判据选出的图像句；回退到工具结果句会指向不存在的工具执行结果。
	t.Run("blank tail user without tool history falls back to image prompt", func(t *testing.T) {
		for name, tail := range map[string]string{
			"empty string": `""`,
			"blank string": `"   "`,
			"empty array":  `[]`,
			"blank text":   `[{"type":"text","text":"  "}]`,
		} {
			t.Run(name, func(t *testing.T) {
				body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
					`{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + wbTestImageDataURL + `"}}]},` +
					`{"role":"assistant","content":"I see a chart."},` +
					`{"role":"user","content":` + tail + `}]}`)
				req, _, _, err := wbParseInbound(body)
				require.NoError(t, err)
				require.Equal(t, "Please answer my latest message.", req.resumePrompt,
					"blank tail must not select the tool-results sentence when no tool history exists")
			})
		}
	})

	// 有工具历史时，空白末条仍应回到工具结果句（既有行为不得因上一修被打断）。
	t.Run("blank tail user with tool history keeps tool-results prompt", func(t *testing.T) {
		body := []byte(`{"model":"deepseek-v4.1-flash","messages":[` +
			`{"role":"user","content":"run"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function",` +
			`"function":{"name":"echo","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"done"},` +
			`{"role":"user","content":"  "}]}`)
		req, _, _, err := wbParseInbound(body)
		require.NoError(t, err)
		require.Equal(t, "Please continue based on the tool execution results above.", req.resumePrompt)
	})

	// 解码接收者复用：每个目标形态都必须独立地把旧 Images 清掉，否则纯文本消息
	// 会被 wbHasImageContent 误判为带图并拖入会话合成路径。
	// 每个分支先向同一接收者装入图片再解目标 content——若沿用「一次装图、后续连解」，
	// 前置步骤已把 Images 清空，后续断言即便是漏写重置也会恒真（假绿）。
	loadImage := func(t *testing.T, m *wbInboundMessage) {
		t.Helper()
		require.NoError(t, json.Unmarshal([]byte(`{"role":"user","content":[`+
			`{"type":"image_url","image_url":{"url":"`+wbTestImageDataURL+`"}}]}`), m))
		require.Len(t, m.Images, 1, "precondition: image must be loaded before each case")
	}

	t.Run("decoder resets images between reuses", func(t *testing.T) {
		cases := []struct {
			name    string
			content string
		}{
			{"string content", `{"role":"user","content":"plain"}`},
			{"null content", `{"role":"user","content":null}`},
			{"omitted content", `{"role":"user"}`},
			{"text-only array", `{"role":"user","content":[{"type":"text","text":"no image"}]}`},
		}
		var m wbInboundMessage
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				loadImage(t, &m)
				require.NoError(t, json.Unmarshal([]byte(c.content), &m))
				require.Empty(t, m.Images, "%s must not carry the previous image", c.name)
			})
		}
	})
}

func wbTestDecodeSessionNodes(t *testing.T, jsonl []byte) []map[string]any {
	t.Helper()
	var nodes []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(jsonl)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var node map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &node))
		nodes = append(nodes, node)
	}
	require.NotEmpty(t, nodes)
	return nodes
}

func wbTestFindUserNodeWithImages(t *testing.T, nodes []map[string]any) map[string]any {
	t.Helper()
	for _, node := range nodes {
		if node["type"] != "message" || node["role"] != "user" {
			continue
		}
		for _, part := range wbTestContentParts(t, node) {
			if part["type"] == "input_image" {
				return node
			}
		}
	}
	t.Fatal("no user node carrying input_image found in synthesized session")
	return nil
}

func wbTestContentParts(t *testing.T, node map[string]any) []map[string]any {
	t.Helper()
	raw, ok := node["content"].([]any)
	require.True(t, ok, "user node content must be a parts array")
	parts := make([]map[string]any, len(raw))
	for i, item := range raw {
		part, ok := item.(map[string]any)
		require.True(t, ok)
		parts[i] = part
	}
	return parts
}
