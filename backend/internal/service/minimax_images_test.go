package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestParseMiniMaxImageRequest(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		request, err := ParseMiniMaxImageRequest([]byte(`{"prompt":"a cat"}`))
		require.NoError(t, err)
		require.Equal(t, DefaultMiniMaxImageModel, request.Model)
		require.Equal(t, 1, request.N)
		require.Empty(t, request.Size)
		require.Empty(t, request.AspectRatio)
	})

	t.Run("explicit fields and size mapping", func(t *testing.T) {
		request, err := ParseMiniMaxImageRequest([]byte(`{"model":"image-01-live","prompt":"  a cat  ","n":3,"size":"1792x1024"}`))
		require.NoError(t, err)
		require.Equal(t, "image-01-live", request.Model)
		require.Equal(t, "a cat", request.Prompt)
		require.Equal(t, 3, request.N)
		require.Equal(t, "16:9", request.AspectRatio)
	})

	t.Run("clamps n and keeps explicit aspect ratio", func(t *testing.T) {
		request, err := ParseMiniMaxImageRequest([]byte(`{"prompt":"a cat","n":99,"aspect_ratio":"9:16"}`))
		require.NoError(t, err)
		require.Equal(t, maxMiniMaxImagesPerRequest, request.N)
		require.Equal(t, "9:16", request.AspectRatio)
	})

	t.Run("provider prefix is stripped", func(t *testing.T) {
		request, err := ParseMiniMaxImageRequest([]byte(`{"model":"minimax/image-01","prompt":"a cat"}`))
		require.NoError(t, err)
		require.Equal(t, "image-01", request.Model)
	})

	t.Run("prompt required", func(t *testing.T) {
		_, err := ParseMiniMaxImageRequest([]byte(`{"model":"image-01"}`))
		require.ErrorContains(t, err, "prompt is required")
	})

	t.Run("invalid json", func(t *testing.T) {
		_, err := ParseMiniMaxImageRequest([]byte(`{`))
		require.ErrorContains(t, err, "invalid JSON body")
	})
}

func TestMinimaxAspectRatioFromSize(t *testing.T) {
	for _, testCase := range []struct {
		size string
		want string
	}{
		{"1024x1024", "1:1"},
		{"1792x1024", "16:9"},
		{"1024x1792", "9:16"},
		{"1536x1024", "16:9"},
		{"1024x1536", "9:16"},
		{"1200x800", "3:2"},
		{"800x1200", "2:3"},
		{"1152x896", "4:3"},
		{"896x1152", "3:4"},
		{"", ""},
		{"auto", ""},
		{"not-a-size", ""},
	} {
		require.Equal(t, testCase.want, minimaxAspectRatioFromSize(testCase.size), "size=%s", testCase.size)
	}
}

func TestIsMiniMaxImageModel(t *testing.T) {
	require.True(t, IsMiniMaxImageModel("image-01"))
	require.True(t, IsMiniMaxImageModel("minimax/image-01-live"))
	require.False(t, IsMiniMaxImageModel("MiniMax-M3.1-Flash"))
	require.False(t, IsMiniMaxImageModel(""))
}

func TestMiniMaxImageDefaultPrice(t *testing.T) {
	billing := &BillingService{}
	require.InDelta(t, DefaultMiniMaxImagePriceUSD, billing.getImageUnitPrice("image-01", ImageBillingSize1K, nil), 1e-9)
	require.InDelta(t, DefaultMiniMaxImagePriceUSD, billing.getImageUnitPrice("minimax/image-01-live", ImageBillingSize2K, nil), 1e-9)

	groupPrice := 0.02
	price := billing.getImageUnitPrice("image-01", ImageBillingSize1K, &ImagePriceConfig{Price1K: &groupPrice})
	require.InDelta(t, groupPrice, price, 1e-9)
}

func TestMinimaxImageURL(t *testing.T) {
	account := &Account{Credentials: map[string]any{"base_url": "https://api.minimaxi.com/v1"}}
	require.Equal(t, "https://api.minimaxi.com/v1/image_generation", minimaxImageURL(account))

	account = &Account{Credentials: map[string]any{}}
	require.Equal(t, DefaultMiniMaxImageBaseURL+"/image_generation", minimaxImageURL(account))

	account = &Account{Credentials: map[string]any{"base_url": "https://relay.example.com/"}}
	require.Equal(t, "https://relay.example.com/v1/image_generation", minimaxImageURL(account))
}

type fakeMiniMaxImageUpstream struct {
	response *http.Response
	lastURL  string
	lastBody []byte
	lastAuth string
}

func (f *fakeMiniMaxImageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	f.lastURL = req.URL.String()
	f.lastAuth = req.Header.Get("Authorization")
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		f.lastBody = body
	}
	return f.response, nil
}

func (f *fakeMiniMaxImageUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return f.Do(req, proxyURL, accountID, accountConcurrency)
}

func newMiniMaxImageTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	return c, recorder
}

func miniMaxImageTestAccount() *Account {
	return &Account{
		ID:       7,
		Platform: PlatformMiniMax,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"api_key":  "test-key",
			"base_url": "https://api.minimaxi.com/v1",
		},
	}
}

func miniMaxImageResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestForwardMiniMaxImage(t *testing.T) {
	upstream := &fakeMiniMaxImageUpstream{
		response: miniMaxImageResponse(http.StatusOK, `{"data":{"image_urls":["https://img.example/1.jpg","https://img.example/2.jpg"]},"base_resp":{"status_code":0,"status_msg":"success"}}`),
	}
	service := &OpenAIGatewayService{httpUpstream: upstream}
	c, recorder := newMiniMaxImageTestContext()

	result, err := service.ForwardMiniMaxImage(context.Background(), c, miniMaxImageTestAccount(), &MiniMaxImageRequest{
		Model: "image-01", Prompt: "a cat", N: 2, Size: "1024x1024", AspectRatio: "1:1",
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.ImageEndpoint)
	require.Equal(t, 2, result.ImageCount)
	require.Equal(t, ImageBillingSize1K, result.ImageSize)
	require.Equal(t, "https://api.minimaxi.com/v1/image_generation", upstream.lastURL)
	require.Equal(t, "Bearer test-key", upstream.lastAuth)

	var upstreamBody map[string]any
	require.NoError(t, json.Unmarshal(upstream.lastBody, &upstreamBody))
	require.Equal(t, "image-01", upstreamBody["model"])
	require.Equal(t, "a cat", upstreamBody["prompt"])
	require.Equal(t, float64(2), upstreamBody["n"])
	require.Equal(t, "url", upstreamBody["response_format"])
	require.Equal(t, "1:1", upstreamBody["aspect_ratio"])

	var response struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Data, 2)
	require.Equal(t, "https://img.example/1.jpg", response.Data[0].URL)
}

func TestForwardMiniMaxImageMapsBusinessError(t *testing.T) {
	upstream := &fakeMiniMaxImageUpstream{
		response: miniMaxImageResponse(http.StatusOK, `{"data":null,"base_resp":{"status_code":2013,"status_msg":"invalid params, prompt is too long"}}`),
	}
	service := &OpenAIGatewayService{httpUpstream: upstream}
	c, _ := newMiniMaxImageTestContext()

	_, err := service.ForwardMiniMaxImage(context.Background(), c, miniMaxImageTestAccount(), &MiniMaxImageRequest{
		Model: "image-01", Prompt: "a cat", N: 1,
	})
	var imageErr *MiniMaxImageError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, http.StatusBadRequest, imageErr.StatusCode)
	require.Equal(t, "invalid_request_error", imageErr.Code)
	require.Contains(t, imageErr.Message, "prompt is too long")
}

func TestForwardMiniMaxImageMapsAuthError(t *testing.T) {
	upstream := &fakeMiniMaxImageUpstream{
		response: miniMaxImageResponse(http.StatusOK, `{"data":null,"base_resp":{"status_code":1004,"status_msg":"login fail"}}`),
	}
	service := &OpenAIGatewayService{httpUpstream: upstream}
	c, _ := newMiniMaxImageTestContext()

	_, err := service.ForwardMiniMaxImage(context.Background(), c, miniMaxImageTestAccount(), &MiniMaxImageRequest{
		Model: "image-01", Prompt: "a cat", N: 1,
	})
	var imageErr *MiniMaxImageError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, http.StatusBadGateway, imageErr.StatusCode)
	require.Equal(t, "upstream_auth_error", imageErr.Code)
}

func TestForwardMiniMaxImageRejectsEmptyOutput(t *testing.T) {
	upstream := &fakeMiniMaxImageUpstream{
		response: miniMaxImageResponse(http.StatusOK, `{"data":{"image_urls":[]},"base_resp":{"status_code":0}}`),
	}
	service := &OpenAIGatewayService{httpUpstream: upstream}
	c, _ := newMiniMaxImageTestContext()

	_, err := service.ForwardMiniMaxImage(context.Background(), c, miniMaxImageTestAccount(), &MiniMaxImageRequest{
		Model: "image-01", Prompt: "a cat", N: 1,
	})
	var imageErr *MiniMaxImageError
	require.ErrorAs(t, err, &imageErr)
	require.Equal(t, "empty_image_output", imageErr.Code)
}

func TestForwardMiniMaxImageRequiresMiniMaxAccount(t *testing.T) {
	service := &OpenAIGatewayService{}
	c, _ := newMiniMaxImageTestContext()
	_, err := service.ForwardMiniMaxImage(context.Background(), c, &Account{Platform: PlatformOpenAI}, &MiniMaxImageRequest{Model: "image-01", Prompt: "a cat", N: 1})
	require.ErrorContains(t, err, "minimax account is required")
}
