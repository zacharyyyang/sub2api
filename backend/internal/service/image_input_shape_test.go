package service

import (
	"strings"
	"testing"
)

// testSecret 是放进请求体里的哨兵字符串：只要它出现在摘要里，就说明正文泄漏了。
const testSecret = "SUPERSECRETTOKEN_zz9"

// 形状指纹只暴露 scheme / MIME / 长度 / 结构标记；任何正文都不得进入结果。
func TestDescribeImageInputs_NeverLeaksContent(t *testing.T) {
	cases := map[string]string{
		"bare base64":              `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + testSecret + `iVBORw0KGgoAAAANSUhEUg"}}]}]}`,
		"data uri":                 `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + testSecret + `"}}]}]}`,
		"bad data uri header":      `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:` + testSecret + `,AAAA"}}]}]}`,
		"remote url with userinfo": `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://alice:` + testSecret + `@example.com/a.png?token=` + testSecret + `"}}]}]}`,
		"file url":                 `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"file:///C:/` + testSecret + `/a.png"}}]}]}`,
		"blob url":                 `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"blob:` + testSecret + `"}}]}]}`,
		"input_image":              `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/jpeg;base64,` + testSecret + `"}]}]}`,
		"image key":                `{"messages":[{"role":"user","content":[{"type":"image","image":{"url":"https://images.example.com/` + testSecret + `/a.png"}}]}]}`,
		"whitespace padded":        `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"  ` + testSecret + `  "}}]}]}`,
	}

	for name, body := range cases {
		got := DescribeImageInputs([]byte(body))
		if got == "" {
			t.Fatalf("%s: expected a shape description", name)
		}
		if strings.Contains(got, testSecret) {
			t.Fatalf("%s: secret leaked into shape description: %q", name, got)
		}
	}
}

func TestDescribeImageInputs_Shapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "data uri keeps validated mime",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			want: []string{"data-uri[image/png;base64]", "len=26"},
		},
		{
			name: "bare base64 classified without content",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"iVBORw0KGgoAAAANSUhEUg"}}]}]}`,
			want: []string{"bare-value", "base64-shaped=true"},
		},
		{
			name: "local file scheme",
			body: `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"file:///tmp/a.png"}]}]}`,
			want: []string{"local-scheme[file]"},
		},
		{
			name: "remote url keeps host only",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png?x=1"}}]}]}`,
			want: []string{"remote-url[https host=example.com]"},
		},
		{
			name: "whitespace flagged",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"  data:image/png;base64,AAAA  "}}]}]}`,
			want: []string{"ws=both"},
		},
		{
			name: "blank url still counted",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":""}}]}]}`,
			want: []string{"image-part[url-blank]"},
		},
		{
			name: "missing url field still counted",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{}}]}]}`,
			want: []string{"image-part[no-url-field]"},
		},
		{
			name: "non-string url still counted",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: []string{"image-part[url-not-string]"},
		},
		{
			name: "no image at all",
			body: `{"messages":[{"role":"user","content":"plain text"}]}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		got := DescribeImageInputs([]byte(tc.body))
		if len(tc.want) == 0 {
			if got != "" {
				t.Fatalf("%s: want empty, got %q", tc.name, got)
			}
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: want %q in %q", tc.name, w, got)
			}
		}
	}
}
