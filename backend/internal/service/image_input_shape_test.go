package service

import "testing"

// 形状指纹只暴露 scheme / MIME / 长度，图片内容永不进入日志。
func TestDescribeImageInputs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "data uri image part",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		},
		{
			name: "bare base64 image part",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"iVBORw0KGgoAAAANSUhEUg"}}]}]}`,
		},
		{
			name: "local scheme",
			body: `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"file:///C:/tmp/a.png"}]}]}`,
		},
		{
			name: "no image",
			body: `{"messages":[{"role":"user","content":"plain text"}]}`,
			want: "",
		},
	}

	for _, tc := range cases {
		got := DescribeImageInputs([]byte(tc.body))
		switch tc.name {
		case "data uri image part":
			if !contains(got, "data-uri[data:image/png;base64]") {
				t.Fatalf("name=%s want data-uri marker, got %q", tc.name, got)
			}
			if !contains(got, "len=") {
				t.Fatalf("name=%s want length marker, got %q", tc.name, got)
			}
		case "bare base64 image part":
			if !contains(got, "bare-value") {
				t.Fatalf("name=%s want bare-value marker, got %q", tc.name, got)
			}
		case "local scheme":
			if !contains(got, "local-scheme[file://") {
				t.Fatalf("name=%s want local-scheme marker, got %q", tc.name, got)
			}
		default:
			if got != tc.want {
				t.Fatalf("name=%s want %q, got %q", tc.name, tc.want, got)
			}
		}
	}
}

func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
