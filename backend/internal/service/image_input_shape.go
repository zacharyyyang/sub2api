package service

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// dataURIMIMEOnly 只接受规范的 data URI 头部：data:<mime>;base64
// 头部不规范时不得把原文前缀当 MIME 输出（原文可能含任意字符）。
var dataURIMIMEOnly = regexp.MustCompile(`^data:([a-z0-9.+-]+/[a-z0-9.+-]+);base64$`)

// base64Shaped 判断是否为 base64 外观（只用于给出分类，不回显内容）。
var base64Shaped = regexp.MustCompile(`^[A-Za-z0-9+/\r\n]+={0,2}$`)

// DescribeImageInputs 返回请求体里每张图片的「形状指纹」，用于定位上游拒收原因。
//
// 隐私边界：只输出 scheme / MIME / 长度 / 结构异常标记，**任何输入正文
// （base64 图像数据、URL、userinfo、查询串）都不得进入返回值**。
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
		// 图片 part 本身就要计数与描述：URL 缺失/为空正是需要诊断的异常形状。
		*out = append(*out, describeImagePart(value))
		return
	}
	collectImageShape(value.Get("content"), out)
}

// describeImagePart 描述一个图片 part。URL 缺失或非字符串时给出结构标记，
// 而不是静默丢弃（静默丢弃会把「异常图片」误报成「没有图片」）。
func describeImagePart(part gjson.Result) string {
	raw, kind := imageURLField(part)
	switch kind {
	case "missing":
		return "image-part[no-url-field]"
	case "not-string":
		return "image-part[url-not-string]"
	}
	if strings.TrimSpace(raw) == "" {
		return fmt.Sprintf("image-part[url-blank] rawLen=%d", len(raw))
	}
	return describeImageURL(raw)
}

// imageURLField 取图片 URL 的原始字符串值（不做 TrimSpace，保证长度与空白可见），
// 并返回字段形态：object-url / string-url / missing / not-string。
func imageURLField(part gjson.Result) (string, string) {
	for _, key := range []string{"image_url", "image"} {
		nested := part.Get(key)
		if !nested.Exists() {
			continue
		}
		if nested.IsObject() {
			inner := nested.Get("url")
			if !inner.Exists() {
				return "", "missing"
			}
			if inner.Type != gjson.String {
				return "", "not-string"
			}
			return inner.String(), "object-url"
		}
		if nested.Type != gjson.String {
			return "", "not-string"
		}
		return nested.String(), "string-url"
	}
	return "", "missing"
}

// describeImageURL 给出一个 URL 的可判定特征，足以区分 data URI / 远程 URL / 裸 base64。
// 任何分支都只输出分类与长度，绝不输出输入正文。
func describeImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	lower := strings.ToLower(trimmed)
	ws := whitespaceMarker(raw, trimmed)

	switch {
	case strings.HasPrefix(lower, "data:"):
		header := trimmed
		if idx := strings.Index(header, ","); idx >= 0 {
			header = header[:idx]
		}
		if m := dataURIMIMEOnly.FindStringSubmatch(strings.ToLower(header)); m != nil {
			return fmt.Sprintf("data-uri[%s;base64] len=%d%s", m[1], len(raw), ws)
		}
		return fmt.Sprintf("data-uri[unrecognized-header] headerLen=%d len=%d%s", len(header), len(raw), ws)
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		scheme := "http"
		if strings.HasPrefix(lower, "https://") {
			scheme = "https"
		}
		return fmt.Sprintf("remote-url[%s host=%s] len=%d%s", scheme, safeHost(trimmed), len(raw), ws)
	case strings.HasPrefix(lower, "blob:"):
		return fmt.Sprintf("local-scheme[blob] len=%d%s", len(raw), ws)
	case strings.HasPrefix(lower, "file:"):
		return fmt.Sprintf("local-scheme[file] len=%d%s", len(raw), ws)
	case strings.HasPrefix(lower, "gs://"), strings.HasPrefix(lower, "s3://"):
		scheme := "gs"
		if strings.HasPrefix(lower, "s3://") {
			scheme = "s3"
		}
		return fmt.Sprintf("object-url[%s] len=%d%s", scheme, len(raw), ws)
	default:
		return fmt.Sprintf("bare-value len=%d base64-shaped=%t%s", len(raw), base64Shaped.MatchString(trimmed), ws)
	}
}

// safeHost 提取主机名，剔除 userinfo（账号/密码）、路径、查询串与 fragment。
// 解析失败时只回 unparsed，不把原文当兜底输出。
func safeHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "unparsed"
	}
	return parsed.Hostname()
}

// whitespaceMarker 标记首尾空白的存在性——上游对空白敏感时需要可区分，
// 但空白以外的原文不得输出。
func whitespaceMarker(raw, trimmed string) string {
	if raw == trimmed {
		return ""
	}
	switch {
	case len(raw) > 0 && len(trimmed) == 0:
		return " ws=all"
	case raw != trimmed && !strings.HasPrefix(raw, trimmed) && !strings.HasSuffix(raw, trimmed):
		return " ws=both"
	case !strings.HasPrefix(raw, trimmed):
		return " ws=leading"
	default:
		return " ws=trailing"
	}
}
