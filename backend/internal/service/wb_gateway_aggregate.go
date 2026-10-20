package service

import "github.com/gin-gonic/gin"

// 本文件是 wb 网关的非流式整包 body 组装层：只做「已解析结果 → OpenAI
// chat.completion JSON」的形状映射，不参与子进程、不参与帧输出逻辑。
// 与流式帧构造（wb_stream_parser.go）分开，两侧各自的契约可独立读、独立测。

// wbToolCallsPayload 组装客户端工具交接回合的整包 body：工具调用按 OpenAI
// tool_calls 透出，finish_reason 固定 "tool_calls"；content 为 nil 表示本回合
// 只有调用、没有正文（OpenAI 对纯调用回合的常规形态）。
func wbToolCallsPayload(model string, calls []WBToolCall, content any, usage wbUsage) gin.H {
	items := make([]any, len(calls))
	for i, call := range calls {
		items[i] = gin.H{
			"id":   call.ID,
			"type": "function",
			"function": gin.H{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		}
	}
	return gin.H{
		"object": "chat.completion",
		"model":  model,
		"choices": []any{gin.H{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": content, "tool_calls": items},
			"finish_reason": "tool_calls",
		}},
		"usage": wbUsageToOpenAI(usage),
	}
}

// wbTextPayload 组装正常文本回合的整包 body。
func wbTextPayload(model, text string, usage wbUsage) gin.H {
	return gin.H{
		"object": "chat.completion",
		"model":  model,
		"choices": []any{gin.H{
			"index":         0,
			"message":       gin.H{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": wbUsageToOpenAI(usage),
	}
}
