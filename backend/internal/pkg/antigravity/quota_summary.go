package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
)

// UserQuotaBucket 官方配额组中的一个窗口（桶）。
// RemainingFraction 用 *float64：nil ⇒ 上游缺字段 / JSON null（= 无数据，调用方按无数据处理）；
// 指向 0.0 ⇒ 合法零值（额度已耗尽，语义上是一条真实数据）。
type UserQuotaBucket struct {
	BucketID          string   `json:"bucketId"`
	DisplayName       string   `json:"displayName"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

// UserQuotaGroup 官方配额组（如 Gemini Models / Claude & GPT）。
type UserQuotaGroup struct {
	DisplayName string            `json:"displayName"`
	Buckets     []UserQuotaBucket `json:"buckets"`
}

// UserQuotaSummaryResponse retrieveUserQuotaSummary 响应。
// 只声明分组与桶两级，忽略上游其余字段（json.Unmarshal 宽松）。
type UserQuotaSummaryResponse struct {
	Groups []UserQuotaGroup `json:"groups"`
}

// FetchUserQuotaSummary 获取 Google 官方配额组额度。
// 与 FetchAvailableModels 同构：固定顺序遍历 BaseURLs（prod→daily），连接错误 /
// 429 / 408 / 404 / 5xx 触发 URL 回退（复用 shouldFallbackToNextURL），
// 成功时 DefaultURLAvailability.MarkSuccess。
// bodyLimit 由调用方传入；响应体超出 bodyLimit 返回错误，不 panic、不截断解析。
func (c *Client) FetchUserQuotaSummary(ctx context.Context, accessToken string, bodyLimit int64) (*UserQuotaSummaryResponse, error) {
	if c == nil || c.httpClient == nil {
		return nil, errors.New("antigravity client is not configured")
	}
	if bodyLimit <= 0 {
		return nil, errors.New("retrieveUserQuotaSummary body limit must be positive")
	}

	reqBody := []byte(`{}`)
	availableURLs := BaseURLs
	var lastErr error
	for urlIdx, baseURL := range availableURLs {
		apiURL := baseURL + "/v1internal:retrieveUserQuotaSummary"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = fmt.Errorf("创建请求失败: %w", err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", GetUserAgentForContext(ctx))

		resp, err := servertiming.Do(c.httpClient, req)
		if err != nil {
			lastErr = fmt.Errorf("retrieveUserQuotaSummary 请求失败: %w", err)
			if shouldFallbackToNextURL(err, 0) && urlIdx < len(availableURLs)-1 {
				log.Printf("[antigravity] retrieveUserQuotaSummary URL fallback: %s -> %s", baseURL, availableURLs[urlIdx+1])
				continue
			}
			return nil, lastErr
		}

		respBodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
		_ = resp.Body.Close() // 立即关闭，避免循环内 defer 导致的资源泄漏
		if err != nil {
			return nil, fmt.Errorf("读取响应失败: %w", err)
		}
		if int64(len(respBodyBytes)) > bodyLimit {
			return nil, fmt.Errorf("响应超过 %d 字节", bodyLimit)
		}

		// 检查是否需要 URL 降级
		if shouldFallbackToNextURL(nil, resp.StatusCode) && urlIdx < len(availableURLs)-1 {
			log.Printf("[antigravity] retrieveUserQuotaSummary URL fallback (HTTP %d): %s -> %s", resp.StatusCode, baseURL, availableURLs[urlIdx+1])
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("retrieveUserQuotaSummary 失败 (HTTP %d): %s", resp.StatusCode, string(respBodyBytes))
		}

		var summaryResp UserQuotaSummaryResponse
		if err := json.Unmarshal(respBodyBytes, &summaryResp); err != nil {
			return nil, fmt.Errorf("响应解析失败: %w", err)
		}

		// 标记成功的 URL，下次优先使用
		DefaultURLAvailability.MarkSuccess(baseURL)
		return &summaryResp, nil
	}

	return nil, lastErr
}

// FetchUserQuotaSummaryForDomain 针对单个指定域获取官方配额组额度（双域旁路探测的下层原语）。
// 与 FetchUserQuotaSummary 区别：固定探测 baseURL 单次、无 URL 回退（回退会把两域混成一域，
// 坏掉「异值并存」）；成功时照常 DefaultURLAvailability.MarkSuccess(baseURL)。
func (c *Client) FetchUserQuotaSummaryForDomain(ctx context.Context, accessToken string, bodyLimit int64, baseURL string) (*UserQuotaSummaryResponse, error) {
	if c == nil || c.httpClient == nil {
		return nil, errors.New("antigravity client is not configured")
	}
	if bodyLimit <= 0 {
		return nil, errors.New("retrieveUserQuotaSummary body limit must be positive")
	}

	reqBody := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1internal:retrieveUserQuotaSummary", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", GetUserAgentForContext(ctx))

	resp, err := servertiming.Do(c.httpClient, req)
	if err != nil {
		return nil, fmt.Errorf("retrieveUserQuotaSummary 请求失败: %w", err)
	}
	respBodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	_ = resp.Body.Close() // 立即关闭，避免 defer 导致的资源泄漏
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	if int64(len(respBodyBytes)) > bodyLimit {
		return nil, fmt.Errorf("响应超过 %d 字节", bodyLimit)
	}

	// 单次无回退：非 200 直接返回错误，不继续尝试下一 URL
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("retrieveUserQuotaSummary 失败 (HTTP %d): %s", resp.StatusCode, string(respBodyBytes))
	}

	var summaryResp UserQuotaSummaryResponse
	if err := json.Unmarshal(respBodyBytes, &summaryResp); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w", err)
	}

	DefaultURLAvailability.MarkSuccess(baseURL)
	return &summaryResp, nil
}
