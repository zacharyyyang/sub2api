package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// DefaultMiniMaxImageBaseURL is used when a MiniMax API-key account has no base_url.
	DefaultMiniMaxImageBaseURL = "https://api.minimaxi.com/v1"
	// DefaultMiniMaxImagePriceUSD is MiniMax's official list price for one image-01 image.
	// Group-level image prices still win when configured; this is only the fallback so the
	// gateway bills at MiniMax's cost instead of the generic per-image default.
	DefaultMiniMaxImagePriceUSD = 0.0035
	// DefaultMiniMaxImageModel is used when the client omits a model.
	DefaultMiniMaxImageModel = "image-01"

	minimaxImageEndpointPath     = "/image_generation"
	maxMiniMaxImagesPerRequest   = 9
	minimaxImageUpstreamBodyCap  = 512
	minimaxImageAuthFailCode     = 1004
	minimaxImageInvalidParamBase = 2000
)

// MiniMaxImageRequest is the normalized OpenAI Images request for the MiniMax platform.
type MiniMaxImageRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	AspectRatio    string
	ResponseFormat string
}

type miniMaxImageUpstreamRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	ResponseFormat string `json:"response_format"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
}

type miniMaxImageUpstreamResponse struct {
	Data *struct {
		ImageURLs []string `json:"image_urls"`
	} `json:"data"`
	BaseResp struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// MiniMaxImageError is a client-facing error produced by the MiniMax image path.
type MiniMaxImageError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *MiniMaxImageError) Error() string { return e.Message }

// NormalizeMiniMaxImageModel strips provider prefixes so both `image-01` and
// `minimax/image-01` reach the upstream model id MiniMax expects.
func NormalizeMiniMaxImageModel(model string) string {
	normalized := strings.TrimSpace(model)
	for {
		lower := strings.ToLower(normalized)
		switch {
		case strings.HasPrefix(lower, "minimax/"):
			normalized = strings.TrimSpace(normalized[len("minimax/"):])
			continue
		case strings.HasPrefix(lower, "minimaxi/"):
			normalized = strings.TrimSpace(normalized[len("minimaxi/"):])
			continue
		}
		break
	}
	return normalized
}

// IsMiniMaxImageModel reports whether the model is one of MiniMax's image models.
func IsMiniMaxImageModel(model string) bool {
	normalized := strings.ToLower(NormalizeMiniMaxImageModel(model))
	return normalized == "image-01" || strings.HasPrefix(normalized, "image-01-")
}

// ParseMiniMaxImageRequest parses an OpenAI Images request body into the fields
// MiniMax's native image endpoint understands.
func ParseMiniMaxImageRequest(body []byte) (*MiniMaxImageRequest, error) {
	var raw struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              *int   `json:"n"`
		Size           string `json:"size"`
		AspectRatio    string `json:"aspect_ratio"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	prompt := strings.TrimSpace(raw.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	model := NormalizeMiniMaxImageModel(raw.Model)
	if model == "" {
		model = DefaultMiniMaxImageModel
	}
	n := 1
	if raw.N != nil {
		n = *raw.N
		if n < 1 {
			n = 1
		}
		if n > maxMiniMaxImagesPerRequest {
			n = maxMiniMaxImagesPerRequest
		}
	}
	size := strings.TrimSpace(raw.Size)
	aspectRatio := strings.TrimSpace(raw.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = minimaxAspectRatioFromSize(size)
	}
	return &MiniMaxImageRequest{
		Model:          model,
		Prompt:         prompt,
		N:              n,
		Size:           size,
		AspectRatio:    aspectRatio,
		ResponseFormat: strings.TrimSpace(raw.ResponseFormat),
	}, nil
}

// minimaxAspectRatioFromSize maps OpenAI image sizes to MiniMax aspect_ratio values.
// An empty result lets MiniMax apply its own default ratio.
func minimaxAspectRatioFromSize(size string) string {
	normalized := strings.ToLower(strings.TrimSpace(size))
	switch normalized {
	case "":
		return ""
	case "1:1", "1024x1024", "512x512":
		return "1:1"
	case "16:9", "1792x1024", "1536x1024", "1280x720":
		return "16:9"
	case "9:16", "1024x1792", "1024x1536", "720x1280":
		return "9:16"
	case "4:3", "1152x896":
		return "4:3"
	case "3:4", "896x1152":
		return "3:4"
	}
	width, height, ok := parseImageBillingDimensions(normalized)
	if !ok {
		return ""
	}
	landscape := float64(width) / float64(height)
	switch {
	case landscape >= 1.55:
		return "16:9"
	case landscape >= 1.4:
		return "3:2"
	case landscape >= 1.2:
		return "4:3"
	case landscape <= 0.65:
		return "9:16"
	case landscape <= 0.72:
		return "2:3"
	case landscape <= 0.84:
		return "3:4"
	default:
		return "1:1"
	}
}

// minimaxImageURL builds the native image endpoint from the account's base_url.
func minimaxImageURL(account *Account) string {
	baseURL := strings.TrimSpace(account.GetCredential("base_url"))
	if baseURL == "" {
		baseURL = DefaultMiniMaxImageBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.Contains(baseURL, "/v1") {
		baseURL += "/v1"
	}
	return baseURL + minimaxImageEndpointPath
}

// SelectMiniMaxImageAccount picks a schedulable MiniMax API-key account from the
// group, preferring the lowest priority value, and acquires its concurrency slot.
// The returned release function must be called when the request ends.
func (s *OpenAIGatewayService) SelectMiniMaxImageAccount(ctx context.Context, groupID *int64) (*Account, func(), error) {
	if s == nil || s.accountRepo == nil || groupID == nil {
		return nil, nil, ErrNoAvailableAccounts
	}
	accounts, err := s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformMiniMax)
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if account.IsMiniMax() && account.Status == StatusActive {
			candidates = append(candidates, account)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority < candidates[j].Priority
		}
		return candidates[i].ID < candidates[j].ID
	})
	for i := range candidates {
		account := &candidates[i]
		if s.concurrencyService == nil {
			return account, func() {}, nil
		}
		result, err := s.concurrencyService.AcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if err != nil || result == nil || !result.Acquired {
			continue
		}
		release := result.ReleaseFunc
		if release == nil {
			release = func() {}
		}
		return account, release, nil
	}
	return nil, nil, ErrNoAvailableAccounts
}

// ForwardMiniMaxImage sends one native MiniMax image generation request, rewrites
// the answer into the OpenAI Images shape, and reports what to bill.
func (s *OpenAIGatewayService) ForwardMiniMaxImage(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	request *MiniMaxImageRequest,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()
	if account == nil || !account.IsMiniMax() {
		return nil, fmt.Errorf("minimax account is required")
	}
	if request == nil {
		return nil, fmt.Errorf("image request is required")
	}
	apiKey := strings.TrimSpace(account.GetCredential("api_key"))
	if apiKey == "" {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       "minimax_api_key_missing",
			Message:    "MiniMax account has no API key",
		}
	}
	upstreamModel := NormalizeMiniMaxImageModel(account.GetMappedModel(request.Model))
	if upstreamModel == "" {
		upstreamModel = request.Model
	}
	upstreamBody, err := json.Marshal(miniMaxImageUpstreamRequest{
		Model:          upstreamModel,
		Prompt:         request.Prompt,
		N:              request.N,
		ResponseFormat: "url",
		AspectRatio:    request.AspectRatio,
	})
	if err != nil {
		return nil, fmt.Errorf("build minimax image request: %w", err)
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, minimaxImageURL(account), bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("build minimax image http request: %w", err)
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusBadGateway,
			Code:       "upstream_error",
			Message:    "MiniMax upstream request failed",
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusBadGateway,
			Code:       "upstream_error",
			Message:    fmt.Sprintf("MiniMax upstream HTTP %d: %s", resp.StatusCode, truncateString(string(respBody), minimaxImageUpstreamBodyCap)),
		}
	}

	var parsed miniMaxImageUpstreamResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusBadGateway,
			Code:       "upstream_invalid_response",
			Message:    "MiniMax returned an unreadable image response",
		}
	}
	if parsed.BaseResp.StatusCode != 0 {
		return nil, miniMaxImageUpstreamError(parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	if parsed.Data == nil || len(parsed.Data.ImageURLs) == 0 {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusBadGateway,
			Code:       "empty_image_output",
			Message:    "MiniMax returned no image output",
		}
	}

	urls := make([]string, 0, len(parsed.Data.ImageURLs))
	images := make([]gin.H, 0, len(parsed.Data.ImageURLs))
	for _, url := range parsed.Data.ImageURLs {
		trimmed := strings.TrimSpace(url)
		if trimmed == "" {
			continue
		}
		urls = append(urls, trimmed)
		images = append(images, gin.H{"url": trimmed})
	}
	if len(images) == 0 {
		return nil, &MiniMaxImageError{
			StatusCode: http.StatusBadGateway,
			Code:       "empty_image_output",
			Message:    "MiniMax returned no image output",
		}
	}

	c.JSON(http.StatusOK, gin.H{"created": time.Now().Unix(), "data": images})

	return &OpenAIForwardResult{
		Model:         request.Model,
		BillingModel:  request.Model,
		UpstreamModel: upstreamModel,
		ImageEndpoint: true,
		ImageCount:    len(urls),
		// MiniMax bills per image regardless of pixel tier; keep the 1K tier so
		// group image_price_1k (when configured) matches that flat price.
		ImageSize: ImageBillingSize1K,
		Duration:  time.Since(startTime),
	}, nil
}

// miniMaxImageUpstreamError maps MiniMax's business status codes onto client-facing errors.
func miniMaxImageUpstreamError(statusCode int, statusMsg string) *MiniMaxImageError {
	message := strings.TrimSpace(statusMsg)
	if message == "" {
		message = "MiniMax rejected the image request"
	}
	switch {
	case statusCode == minimaxImageAuthFailCode:
		return &MiniMaxImageError{StatusCode: http.StatusBadGateway, Code: "upstream_auth_error", Message: message}
	case statusCode >= minimaxImageInvalidParamBase:
		return &MiniMaxImageError{StatusCode: http.StatusBadRequest, Code: "invalid_request_error", Message: message}
	default:
		return &MiniMaxImageError{StatusCode: http.StatusBadGateway, Code: "upstream_error", Message: message}
	}
}
