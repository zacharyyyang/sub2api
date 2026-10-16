import { describe, expect, it, vi } from 'vitest'

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

import {
  allModels,
  buildModelMappingObject,
  getModelsByPlatform,
  qoderModels,
  splitModelMappingObject
} from '../useModelWhitelist'

const EXPECTED_QODER_MODELS = [
  'sonus-fable5.1',
  'cantus-opus5.5',
  'ultimate-gpt6.1sol',
  'Qwen3.8-Max',
  'Qwen3.8-Flash',
  'Qwen3.7-Max',
  'Qwen3.7-Plus',
  'Kimi-K3',
  'Kimi-K2.8-Preview',
  'GLM-5.3',
  'GLM-5.3-Flash',
  'DeepSeek-V4-Pro',
  'DeepSeek-Flash',
  'MiniMax-M3'
]

describe('useModelWhitelist Qoder platform', () => {
  it('qoderModels 与后端 domain.QoderModelWhitelist 14 个模型逐字一致', () => {
    expect(qoderModels).toEqual(EXPECTED_QODER_MODELS)
    expect(qoderModels).toHaveLength(14)
    expect(getModelsByPlatform('qoder')).toEqual(EXPECTED_QODER_MODELS)
  })

  it('allModels 包含全部 14 个 Qoder 模型且无重复选项', () => {
    const allValues = allModels.map(opt => opt.value)
    for (const model of EXPECTED_QODER_MODELS) {
      expect(allValues).toContain(model)
    }
    const uniqueValues = new Set(allValues)
    expect(allValues.length).toBe(uniqueValues.size)
  })

  it('Qoder 模型支持 whitelist / mapping / combined 构建与拆分', () => {
    const mapping = buildModelMappingObject(
      'combined',
      ['sonus-fable5.1', 'Kimi-K3'],
      [{ from: 'claude-3-7-sonnet', to: 'sonus-fable5.1' }]
    )
    expect(mapping).toEqual({
      'sonus-fable5.1': 'sonus-fable5.1',
      'Kimi-K3': 'Kimi-K3',
      'claude-3-7-sonnet': 'sonus-fable5.1'
    })

    const split = splitModelMappingObject(mapping)
    expect(split.allowedModels).toEqual(['sonus-fable5.1', 'Kimi-K3'])
    expect(split.modelMappings).toEqual([{ from: 'claude-3-7-sonnet', to: 'sonus-fable5.1' }])
  })
})
