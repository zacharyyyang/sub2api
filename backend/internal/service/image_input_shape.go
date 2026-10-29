package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// DescribeImageInputs 返回请求体里每张图片的「形状指纹」，用于定位上游拒收原因。
//
// 只描述形状，不携带内容：记录 URL 的 scheme 前缀（截到第一个逗号）、总长度、
// 以及 data URI 声明的 MIME。图片本身（base64 正文）永不进入日志。
//
// 返回空串表示请求里没有图片输入。
func DescribeImageInputs(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	var parts []string
	collectImageShape(gjson.GetBytes(body, "messages"), &parts)
	collectImageShape(gjson.GetBytes(body, "input"), &parts)
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, "; ")
	if len(out) > 600 {
		out = out[:600] + "…"
	}
	return fmt.Sprintf("%d image(s): %s", len(parts), out)
}

func collectImageShape(value gjson.Result, out *[]string) {
	if !value.Exists() {
		return
	}
	if value.IsArray() {
		value.ForEach(func(_, item gjson.Result) bool {
			collectImageShape(item, out)
			return true
		})
		return
	}
	if !value.IsObject() {
		return
	}
	kind := strings.TrimSpace(value.Get("type").String())
	if kind == "image_url" || kind == "input_image" || kind == "image" {
		if url := imageURLFromPart(value); url != "" {
			*out = append(*out, describeImageURL(url))
		}
		return
	}
	collectImageShape(value.Get("content"), out)
}

func imageURLFromPart(part gjson.Result) string {
	if nested := part.Get("image_url"); nested.Exists() {
		if nested.IsObject() {
			if u := strings.TrimSpace(nested.Get("url").String()); u != "" {
				return u
			}
			return ""
		}
		return strings.TrimSpace(nested.String())
	}
	if nested := part.Get("image"); nested.Exists() {
		if nested.IsObject() {
			return strings.TrimSpace(nested.Get("url").String())
		}
		return strings.TrimSpace(nested.String())
	}
	return ""
}

// describeImageURL 给出一个 URL 的可判定特征，足以区分 data URI / 远程 URL / 裸 base64。
func describeImageURL(url string) string {
	lower := strings.ToLower(url)
	switch {
	case strings.HasPrefix(lower, "data:"):
		mime := url
		if idx := strings.Index(mime, ","); idx >= 0 {
			mime = mime[:idx]
		}
		return fmt.Sprintf("data-uri[%s] len=%d", truncateImageShape(mime, 40), len(url))
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return fmt.Sprintf("remote-url[%s] len=%d", truncateImageShape(hostOfURL(url), 40), len(url))
	case strings.HasPrefix(lower, "blob:"), strings.HasPrefix(lower, "file:"):
		return fmt.Sprintf("local-scheme[%s] len=%d", truncateImageShape(url, 40), len(url))
	case strings.HasPrefix(lower, "gs://"), strings.HasPrefix(lower, "s3://"):
		return fmt.Sprintf("object-url[%s] len=%d", truncateImageShape(url, 40), len(url))
	default:
		return fmt.Sprintf("bare-value len=%d head=%q", len(url), truncateImageShape(url, 12))
	}
}

func hostOfURL(url string) string {
	rest := url
	if idx := strings.Index(rest, "://"); idx >= 0 {
		rest = rest[idx+3:]
	}
	if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
		rest = rest[:idx]
	}
	return rest
}

func truncateImageShape(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
