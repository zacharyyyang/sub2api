import { describe, it, expect, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import QoderCredentialFields from '../QoderCredentialFields.vue'
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
    useI18n: () => ({
      t: (key: string) => (key === 'admin.accounts.leaveEmptyToKeep' ? '留空以保持当前密钥' : key)
    })
  }
})

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      update: updateAccountMock,
      checkMixedChannelRisk: checkMixedChannelRiskMock
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isSimpleMode: true })
}))

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: Boolean },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: { type: Array, default: () => [] },
    modelMappings: { type: Array, default: () => [] },
    mappingMode: String,
    platform: String
  },
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

describe('QoderCredentialFields.vue', () => {
  describe('创建模式 (isEdit = false)', () => {
    it('四项必填凭证为空时 isValid 返回 false 并显示错误', async () => {
      const wrapper = mount(QoderCredentialFields, {
        props: { modelValue: {}, isEdit: false }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()

      expect(wrapper.text()).toContain('请填写 Access Token (dt-...)')
      expect(wrapper.text()).toContain('请填写 Refresh Token (drt-...)')
      expect(wrapper.text()).toContain('请填写 User ID (uid)')
      expect(wrapper.text()).toContain('请填写 Organization ID (org_id)')
    })

    it('必填与高级设置 9 字段全部填写且毫秒时间戳合法时 collect 完整返回', async () => {
      const wrapper = mount(QoderCredentialFields, {
        props: { modelValue: {}, isEdit: false }
      })

      await wrapper.get('[data-testid="qoder-advanced-toggle"]').trigger('click')
      const inputs = wrapper.findAll('input')
      await inputs[0].setValue('qoder-at')
      await inputs[1].setValue('qoder-rt')
      await inputs[2].setValue('qoder-uid')
      await inputs[3].setValue('qoder-org')
      await inputs[4].setValue('machine-123')
      await inputs[5].setValue('umid-456')
      await inputs[6].setValue('mac_arm64')
      await inputs[7].setValue('1760000000000')
      await inputs[8].setValue('1770000000000')

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(true)
      expect(vm.collect()).toEqual({
        access_token: 'qoder-at',
        refresh_token: 'qoder-rt',
        uid: 'qoder-uid',
        org_id: 'qoder-org',
        machine_id: 'machine-123',
        umid_token: 'umid-456',
        machine_type: 'mac_arm64',
        token_expires_at: '1760000000000',
        refresh_token_expires_at: '1770000000000'
      })
    })

    it('非 13 位毫秒级正整数时间戳触发校验错误', async () => {
      const wrapper = mount(QoderCredentialFields, {
        props: {
          modelValue: {
            access_token: 'qoder-at',
            refresh_token: 'qoder-rt',
            uid: 'qoder-uid',
            org_id: 'qoder-org',
            token_expires_at: '1760000000',
            refresh_token_expires_at: 'abc'
          },
          isEdit: false
        }
      })

      await wrapper.get('[data-testid="qoder-advanced-toggle"]').trigger('click')
      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()
      expect(wrapper.text()).toContain('需为 13 位毫秒级正整数时间戳')
    })
  })

  describe('编辑模式 (isEdit = true)', () => {
    it('敏感字段脱敏留空且 credentialsStatus 已存在时通过校验且 collect 输出空串以保留后端原值', async () => {
      const wrapper = mount(QoderCredentialFields, {
        props: {
          modelValue: { uid: 'existing-uid', org_id: 'existing-org', machine_id: 'existing-machine' },
          credentialsStatus: { has_access_token: true, has_refresh_token: true, has_umid_token: true },
          isEdit: true
        }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(true)
      await wrapper.vm.$nextTick()
      expect(wrapper.text()).toContain('留空以保持当前密钥')

      const collected = vm.collect()
      expect(collected.uid).toBe('existing-uid')
      expect(collected.org_id).toBe('existing-org')
      expect(collected.machine_id).toBe('existing-machine')
      expect(collected.access_token).toBe('')
      expect(collected.refresh_token).toBe('')
      expect(collected.umid_token).toBe('')
    })

    it('编辑模式下 credentialsStatus 明确为 false 且未填写敏感字段时拦截报错', async () => {
      const wrapper = mount(QoderCredentialFields, {
        props: {
          modelValue: { uid: 'existing-uid', org_id: 'existing-org' },
          credentialsStatus: { has_access_token: false, has_refresh_token: false },
          isEdit: true
        }
      })

      const vm = wrapper.vm as any
      expect(vm.isValid()).toBe(false)
      await wrapper.vm.$nextTick()
      expect(wrapper.text()).toContain('请填写 Access Token')
      expect(wrapper.text()).toContain('请填写 Refresh Token')
    })
  })

  describe('CreateAccountModal / EditAccountModal Qoder 分支集成', () => {
    beforeEach(() => {
      createAccountMock.mockReset().mockResolvedValue({ id: 88, platform: 'qoder', type: 'apikey' })
      updateAccountMock.mockReset().mockResolvedValue({ id: 88, platform: 'qoder', type: 'apikey' })
      checkMixedChannelRiskMock.mockReset().mockResolvedValue({ has_risk: false })
      showErrorMock.mockReset()
    })

    it('CreateAccountModal 选中 Qoder 平台提交时校验必填项并组装 qoder credentials', async () => {
      const wrapper = mount(CreateAccountModal, {
        props: { show: true, proxies: [], groups: [] },
        global: { stubs: modalStubs }
      })

      await wrapper.get('[data-testid="platform-tab-qoder"]').trigger('click')
      await wrapper.get('form#create-account-form input[type="text"]').setValue('Qoder Account')

      const qoderComp = wrapper.findComponent(QoderCredentialFields)
      expect(qoderComp.exists()).toBe(true)
      const qoderInputs = qoderComp.findAll('input')
      await qoderInputs[0].setValue('at-new')
      await qoderInputs[1].setValue('rt-new')
      await qoderInputs[2].setValue('uid-new')
      await qoderInputs[3].setValue('org-new')

      await wrapper.get('form#create-account-form').trigger('submit.prevent')
      await flushPromises()

      expect(createAccountMock).toHaveBeenCalledTimes(1)
      const payload = createAccountMock.mock.calls[0]?.[0]
      expect(payload.platform).toBe('qoder')
      expect(payload.type).toBe('apikey')
      expect(payload.credentials).toMatchObject({
        access_token: 'at-new',
        refresh_token: 'rt-new',
        uid: 'uid-new',
        org_id: 'org-new'
      })
      expect(Object.keys(payload.credentials.model_mapping)).toHaveLength(14)
      expect(payload.credentials.model_mapping['sonus-fable5.1']).toBe('sonus-fable5.1')
    })

    it('EditAccountModal 保存 Qoder 账号时剔除留空的脱敏/选填字段以保留后端原值', async () => {
      const account = {
        id: 88,
        name: 'Qoder Existing',
        notes: '',
        platform: 'qoder',
        type: 'apikey',
        credentials: {
          uid: 'uid-keep',
          org_id: 'org-keep',
          model_mapping: { 'sonus-fable5.1': 'sonus-fable5.1' }
        },
        credentials_status: { has_access_token: true, has_refresh_token: true, has_umid_token: true },
        extra: {},
        proxy_id: null,
        concurrency: 2,
        priority: 1,
        rate_multiplier: 1,
        status: 'active',
        group_ids: [],
        expires_at: null,
        auto_pause_on_expired: false
      } as any

      const wrapper = mount(EditAccountModal, {
        props: { show: true, account, proxies: [], groups: [] },
        global: { stubs: modalStubs }
      })

      await wrapper.get('form#edit-account-form').trigger('submit.prevent')
      await flushPromises()

      expect(updateAccountMock).toHaveBeenCalledTimes(1)
      const savedCreds = updateAccountMock.mock.calls[0]?.[1]?.credentials
      expect(savedCreds).toEqual({
        uid: 'uid-keep',
        org_id: 'org-keep',
        model_mapping: { 'sonus-fable5.1': 'sonus-fable5.1' }
      })
      expect(savedCreds).not.toHaveProperty('access_token')
      expect(savedCreds).not.toHaveProperty('refresh_token')
      expect(savedCreds).not.toHaveProperty('umid_token')
    })
  })
})
