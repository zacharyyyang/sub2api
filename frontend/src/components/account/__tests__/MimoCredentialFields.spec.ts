import { describe, it, expect, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import MimoCredentialFields from '../MimoCredentialFields.vue'
import CreateAccountModal from '../CreateAccountModal.vue'
import EditAccountModal from '../EditAccountModal.vue'

const { createAccountMock, updateAccountMock, checkMixedChannelRiskMock, showErrorMock } = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  updateAccountMock: vi.fn(),
  checkMixedChannelRiskMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => (key === 'admin.accounts.leaveEmptyToKeep' ? '留空以保持当前密钥' : key) })
  }
})

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: { create: createAccountMock, update: updateAccountMock, checkMixedChannelRisk: checkMixedChannelRiskMock },
    settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }), getSettings: vi.fn().mockResolvedValue({}) },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn() }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: showErrorMock, showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: true }) }))

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: Boolean },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: { modelValue: { type: Array, default: () => [] }, modelMappings: { type: Array, default: () => [] }, mappingMode: String, platform: String },
  emits: ['update:modelValue', 'update:modelMappings', 'update:mappingMode'],
  template: '<div data-testid="model-whitelist-selector" />'
})

const modalStubs = {
  BaseDialog: BaseDialogStub,
  OAuthAuthorizationFlow: true,
  ConfirmDialog: true,
  Select: true,
  Icon: true,
  PlatformIcon: true,
  ProxySelector: true,
  ProxyAdBanner: true,
  GroupSelector: true,
  ModelWhitelistSelector: ModelWhitelistSelectorStub,
  QuotaLimitCard: true
}

describe('MimoCredentialFields.vue', () => {
  it('创建模式必填校验与 6 字段（含高级设置）双向绑定收集', async () => {
    const wrapper = mount(MimoCredentialFields, { props: { modelValue: {}, isEdit: false } })
    const vm = wrapper.vm as any
    expect(vm.isValid()).toBe(false)
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).toContain('请填写 Xiaomi User ID (user_id)')
    expect(wrapper.text()).toContain('请填写 Pass Token (pass_token) 或 Service Token (service_token)（至少一项）')

    await wrapper.get('[data-testid="mimo-advanced-toggle"]').trigger('click')
    const inputs = wrapper.findAll('input')
    await inputs[0].setValue('100000001')
    await inputs[1].setValue('V1:fake_pass_token==')
    await inputs[2].setValue('fake_service_token_001')
    await inputs[3].setValue('fake_c_user_id_001')
    await inputs[4].setValue('fake_mimo_ph_001')
    await inputs[5].setValue('https://mimo-server-cn.xiaomimimo.com')

    expect(vm.isValid()).toBe(true)
    expect(vm.collect()).toEqual({
      user_id: '100000001',
      pass_token: 'V1:fake_pass_token==',
      service_token: 'fake_service_token_001',
      c_user_id: 'fake_c_user_id_001',
      mimo_ph: 'fake_mimo_ph_001',
      base_url: 'https://mimo-server-cn.xiaomimimo.com'
    })
  })

  it('编辑模式敏感字段留空且 credentialsStatus 已存在时保留原值', async () => {
    const wrapper = mount(MimoCredentialFields, {
      props: {
        modelValue: { user_id: '100000001', c_user_id: 'fake_c_user_id_001', base_url: 'https://mimo-server-cn.xiaomimimo.com' },
        credentialsStatus: { has_pass_token: true, has_service_token: false, has_mimo_ph: true },
        isEdit: true
      }
    })
    const vm = wrapper.vm as any
    expect(vm.isValid()).toBe(true)
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).toContain('留空以保持当前密钥')
    expect(vm.collect()).toMatchObject({ user_id: '100000001', c_user_id: 'fake_c_user_id_001', pass_token: '', service_token: '', mimo_ph: '' })
  })

  describe('CreateAccountModal / EditAccountModal MiMo 分支集成', () => {
    beforeEach(() => {
      createAccountMock.mockReset().mockResolvedValue({ id: 99, platform: 'mimo', type: 'apikey' })
      updateAccountMock.mockReset().mockResolvedValue({ id: 99, platform: 'mimo', type: 'apikey' })
      checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
      showErrorMock.mockReset()
    })

    it('CreateAccountModal 选中 MiMo 平台提交时预填 6 个官方模型并组装 mimo credentials', async () => {
      const wrapper = mount(CreateAccountModal, { props: { show: true, proxies: [], groups: [] }, global: { stubs: modalStubs } })
      await wrapper.get('[data-testid="platform-tab-mimo"]').trigger('click')
      await wrapper.get('form#create-account-form input[type="text"]').setValue('MiMo Account')
      const mimoInputs = wrapper.findComponent(MimoCredentialFields).findAll('input')
      await mimoInputs[0].setValue('100000001')
      await mimoInputs[1].setValue('V1:fake_pass_token==')
      await wrapper.get('form#create-account-form').trigger('submit.prevent')
      await flushPromises()

      expect(createAccountMock).toHaveBeenCalledTimes(1)
      const payload = createAccountMock.mock.calls[0]?.[0]
      expect(payload.platform).toBe('mimo')
      expect(payload.type).toBe('apikey')
      expect(payload.credentials).toMatchObject({ user_id: '100000001', pass_token: 'V1:fake_pass_token==' })
      expect(Object.keys(payload.credentials.model_mapping)).toHaveLength(6)
    })

    it('EditAccountModal 保存 MiMo 账号时剔除留空的敏感字段以保留后端原值', async () => {
      const account = {
        id: 99, name: 'MiMo Existing', notes: '', platform: 'mimo', type: 'apikey',
        credentials: { user_id: '100000001', c_user_id: 'fake_c_user_id_001', model_mapping: { 'mimo-v2.6-pro': 'mimo-v2.6-pro' } },
        credentials_status: { has_pass_token: true, has_service_token: true, has_mimo_ph: true },
        extra: {}, proxy_id: null, concurrency: 2, priority: 1, rate_multiplier: 1, status: 'active', group_ids: [], expires_at: null, auto_pause_on_expired: false
      } as any
      const wrapper = mount(EditAccountModal, { props: { show: true, account, proxies: [], groups: [] }, global: { stubs: modalStubs } })
      await wrapper.get('form#edit-account-form').trigger('submit.prevent')
      await flushPromises()

      expect(updateAccountMock).toHaveBeenCalledTimes(1)
      const savedCreds = updateAccountMock.mock.calls[0]?.[1]?.credentials
      expect(savedCreds).toEqual({ user_id: '100000001', c_user_id: 'fake_c_user_id_001', model_mapping: { 'mimo-v2.6-pro': 'mimo-v2.6-pro' } })
    })
  })
})
