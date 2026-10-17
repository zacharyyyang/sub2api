import { describe, expect, it, vi } from 'vitest'

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

import {
  allModels,
  buildModelMappingObject,
  getModelsByPlatform,
  mimoModels,
  splitModelMappingObject
} from '../useModelWhitelist'

const EXPECTED_MIMO_MODELS = [
  'mimo-v2.6-pro',
  'mimo-v2.6-pro-ultraspeed',
  'mimo-v2.6-flash',
  'mimo-v2-pro',
  'mimo-v2-omni',
  'mimo-v2-flash'
]

describe('useModelWhitelist MiMo platform', () => {
  it('mimoModels 与后端 domain.MimoModelWhitelist 6 个官方可读模型逐字一致', () => {
    expect(mimoModels).toEqual(EXPECTED_MIMO_MODELS)
    expect(mimoModels).toHaveLength(6)
    expect(getModelsByPlatform('mimo')).toEqual(EXPECTED_MIMO_MODELS)
  })

  it('allModels 包含全部 6 个 MiMo 模型且无重复选项，且不干扰 opencode_go 的 mimo-v2.5', () => {
    const allValues = allModels.map(opt => opt.value)
    for (const model of EXPECTED_MIMO_MODELS) {
      expect(allValues).toContain(model)
    }
    const uniqueValues = new Set(allValues)
    expect(allValues.length).toBe(uniqueValues.size)
    expect(getModelsByPlatform('opencode_go')).toContain('mimo-v2.5')
  })

  it('MiMo 模型支持 whitelist / mapping / combined 构建与拆分', () => {
    const mapping = buildModelMappingObject(
      'combined',
      ['mimo-v2.6-pro', 'mimo-v2-omni'],
      [{ from: 'claude-sonnet-4-6', to: 'mimo-v2.6-pro' }]
    )
    expect(mapping).toEqual({
      'mimo-v2.6-pro': 'mimo-v2.6-pro',
      'mimo-v2-omni': 'mimo-v2-omni',
      'claude-sonnet-4-6': 'mimo-v2.6-pro'
    })

    const split = splitModelMappingObject(mapping)
    expect(split.allowedModels).toEqual(['mimo-v2.6-pro', 'mimo-v2-omni'])
    expect(split.modelMappings).toEqual([{ from: 'claude-sonnet-4-6', to: 'mimo-v2.6-pro' }])
  })
})
