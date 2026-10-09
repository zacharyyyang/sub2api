import { describe, it, expect, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import WbCredentialFields from '../WbCredentialFields.vue'

// Mock vue-i18n
vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => {
      if (key === 'admin.accounts.leaveEmptyToKeep') return '留空以保持当前密钥'
      return key
    }
  })
}))

describe('WbCredentialFields.vue', () => {
  describe('创建模式 (isEdit = false)', () => {
    it('四项必填凭证为空时 isValid 返回 false 并显示错误', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {},
          isEdit: false
        }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()

      // 错误提示出现在四个必填字段
      expect(wrapper.text()).toContain('请填写 Client ID')
      expect(wrapper.text()).toContain('请填写 Client Secret')
      expect(wrapper.text()).toContain('请填写 PT Key')
      expect(wrapper.text()).toContain('请填写 Enterprise ID')
    })

    it('四项必填凭证全部填写后 isValid 返回 true 并成功 collect', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {},
          isEdit: false
        }
      })

      const inputs = wrapper.findAll('input')
      // [0] client_id, [1] client_secret, [2] pt_key, [3] enterprise_id, [4] cli_path
      await inputs[0].setValue('my-client-id')
      await inputs[1].setValue('my-secret')
      await inputs[2].setValue('my-pt-key')
      await inputs[3].setValue('my-ent-id')

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(true)

      const collected = vm.collect()
      expect(collected).toEqual({
        client_id: 'my-client-id',
        client_secret: 'my-secret',
        pt_key: 'my-pt-key',
        enterprise_id: 'my-ent-id',
        cli_path: ''
      })
    })
  })

  describe('编辑模式 (isEdit = true)', () => {
    it('敏感字段脱敏留空且已有凭证时，isValid 返回 true 且不提示错误', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {
            client_id: 'existing-client-id',
            enterprise_id: 'existing-ent-id'
          },
          credentialsStatus: {
            has_client_secret: true,
            has_pt_key: true
          },
          isEdit: true
        }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(true)
      await wrapper.vm.$nextTick()

      expect(wrapper.text()).not.toContain('请填写 Client Secret')
      expect(wrapper.text()).not.toContain('请填写 PT Key')

      // placeholder 与 hint 包含「留空以保持当前密钥」
      expect(wrapper.text()).toContain('留空以保持当前密钥')

      // collect() 时未修改的敏感字段输出为空串，以便外层 delete 保持后端原值
      const collected = vm.collect()
      expect(collected.client_id).toBe('existing-client-id')
      expect(collected.enterprise_id).toBe('existing-ent-id')
      expect(collected.client_secret).toBe('')
      expect(collected.pt_key).toBe('')
    })

    it('编辑模式下用户输入新密钥时，正确采集新密钥', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {
            client_id: 'existing-client-id',
            enterprise_id: 'existing-ent-id'
          },
          credentialsStatus: {
            has_client_secret: true,
            has_pt_key: true
          },
          isEdit: true
        }
      })

      const inputs = wrapper.findAll('input')
      // 输入新的 Client Secret
      await inputs[1].setValue('brand-new-secret')

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(true)

      const collected = vm.collect()
      expect(collected.client_id).toBe('existing-client-id')
      expect(collected.client_secret).toBe('brand-new-secret')
      expect(collected.pt_key).toBe('') // PT Key 留空
    })

    it('编辑模式下清空非敏感必填项 Client ID 时，isValid 拦截并报错', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {
            client_id: 'existing-client-id',
            enterprise_id: 'existing-ent-id'
          },
          credentialsStatus: {
            has_client_secret: true,
            has_pt_key: true
          },
          isEdit: true
        }
      })

      const inputs = wrapper.findAll('input')
      await inputs[0].setValue('') // 清空 client_id

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()

      expect(wrapper.text()).toContain('请填写 Client ID')
    })

    it('编辑模式下若 credentialsStatus 明确报告无敏感凭据且未填写，isValid 拦截', async () => {
      const wrapper = mount(WbCredentialFields, {
        props: {
          modelValue: {
            client_id: 'existing-client-id',
            enterprise_id: 'existing-ent-id'
          },
          credentialsStatus: {
            has_client_secret: false,
            has_pt_key: false
          },
          isEdit: true
        }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()

      expect(wrapper.text()).toContain('请填写 Client Secret')
      expect(wrapper.text()).toContain('请填写 PT Key')
    })
  })
})
