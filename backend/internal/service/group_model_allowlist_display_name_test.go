package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// 显示名映射的归一化：键与值都 TrimSpace，空键 / 空值丢弃，其余原样保留。
// 显示名独立于 Models / Enabled：白名单关闭或模型列表为空时映射照旧归一化后保留。
func TestNormalizeGroupModelAllowlistDisplayNames(t *testing.T) {
	t.Run("trims keys and values", func(t *testing.T) {
		got, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
			Enabled:      true,
			Models:       []string{"claude-a"},
			DisplayNames: map[string]string{"  claude-a  ": "  DeepSeek · V4.1 Flash  "},
		})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"claude-a": "DeepSeek · V4.1 Flash"}, got.DisplayNames)
	})

	t.Run("drops empty key and empty value", func(t *testing.T) {
		got, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
			DisplayNames: map[string]string{"": "x", "claude-a": "   "},
		})
		require.NoError(t, err)
		require.Empty(t, got.DisplayNames)
		require.Nil(t, got.DisplayNames)
	})

	t.Run("keeps display names when allowlist is disabled and models are empty", func(t *testing.T) {
		got, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
			Enabled:      false,
			DisplayNames: map[string]string{"claude-mythos-ds": "甲"},
		})
		require.NoError(t, err)
		require.False(t, got.Enabled)
		require.Empty(t, got.Models)
		require.Equal(t, map[string]string{"claude-mythos-ds": "甲"}, got.DisplayNames)
	})

	t.Run("keeps display names for model ids outside the allowlist", func(t *testing.T) {
		got, err := normalizeGroupModelAllowlist(GroupModelAllowlist{
			Enabled:      true,
			Models:       []string{"claude-a"},
			DisplayNames: map[string]string{"absent": "X", "*": "通配不动"},
		})
		require.NoError(t, err)
		require.Equal(t, map[string]string{"absent": "X", "*": "通配不动"}, got.DisplayNames)
	})
}

// DisplayNameFor 精确匹配模型 id 原文：命中返回配置值与 true，未命中与空值都返回 false。
func TestGroupModelAllowlistDisplayNameFor(t *testing.T) {
	allowlist := GroupModelAllowlist{
		DisplayNames: map[string]string{"claude-mythos-ds": "DeepSeek · V4.1 Flash", "empty": ""},
	}

	name, ok := allowlist.DisplayNameFor("claude-mythos-ds")
	require.True(t, ok)
	require.Equal(t, "DeepSeek · V4.1 Flash", name)

	for _, miss := range []string{"claude-other", "claude-mythos-ds[1m]", "claude-fable-5-claude-mythos-ds", "CLAUDE-MYTHOS-DS"} {
		name, ok := allowlist.DisplayNameFor(miss)
		require.Falsef(t, ok, "lookup %q must not hit: display names are exact-match on the raw model id", miss)
		require.Empty(t, name)
	}

	_, ok = allowlist.DisplayNameFor("empty")
	require.False(t, ok, "empty stored value counts as no configuration")

	var zero GroupModelAllowlist
	_, ok = zero.DisplayNameFor("claude-mythos-ds")
	require.False(t, ok)
}

// FromDomain / DomainGroupModelAllowlist 逐字段拷贝，display_names 两个方向都不能丢。
func TestGroupModelAllowlistDisplayNameDomainRoundTrip(t *testing.T) {
	svc := GroupModelAllowlist{
		Enabled:      true,
		Models:       []string{"claude-a"},
		DisplayNames: map[string]string{"claude-a": "甲"},
	}

	domainCfg := DomainGroupModelAllowlist(svc)
	require.Equal(t, map[string]string{"claude-a": "甲"}, domainCfg.DisplayNames)

	back := GroupModelAllowlistFromDomain(domainCfg)
	require.Equal(t, map[string]string{"claude-a": "甲"}, back.DisplayNames)
	require.True(t, back.Enabled)
	require.Equal(t, []string{"claude-a"}, back.Models)

	empty := GroupModelAllowlistFromDomain(domain.GroupModelAllowlist{})
	require.Nil(t, empty.DisplayNames, "存量分组读出来必须是 nil，输出与改动前一致")
}
