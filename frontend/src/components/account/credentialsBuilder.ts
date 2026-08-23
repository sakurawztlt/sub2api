export function applyInterceptWarmup(
  credentials: Record<string, unknown>,
  enabled: boolean,
  mode: 'create' | 'edit'
): void {
  if (enabled) {
    credentials.intercept_warmup_requests = true
  } else if (mode === 'edit') {
    delete credentials.intercept_warmup_requests
  }
}

export const ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY = 'antigravity_project_id'

export function applyAntigravityProjectID(
  credentials: Record<string, unknown>,
  projectId: string,
  mode: 'create' | 'edit'
): void {
  const trimmed = projectId.trim()
  if (trimmed) {
    credentials[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY] = trimmed
  } else if (mode === 'edit') {
    delete credentials[ANTIGRAVITY_PROJECT_ID_CREDENTIAL_KEY]
  }
}

// ========== 请求头覆写（API-key 平台 + grok 的 api_key/oauth 账号） ==========

export const HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY = 'header_override_enabled'
export const HEADER_OVERRIDES_CREDENTIAL_KEY = 'header_overrides'

export interface HeaderOverrideRow {
  name: string
  value: string
}

/** 请求头覆写资格（与后端 IsHeaderOverrideEligible 保持一致） */
export function isHeaderOverrideCapable(platform: string, type: string): boolean {
  if (
    platform === 'anthropic' ||
    platform === 'openai' ||
    platform === 'kimi' ||
    platform === 'zhipu' ||
    platform === 'deepseek'
  ) {
    return type === 'apikey'
  }
  if (platform === 'grok') {
    return type === 'apikey' || type === 'oauth'
  }
  return false
}

/** 请求头覆写支持的平台，保留给只关心平台的旧调用方。 */
export function isHeaderOverridePlatform(platform: string): boolean {
  return platform === 'anthropic' || platform === 'openai' || platform === 'grok'
}

/** 禁止覆写的请求头（与后端 headerOverrideBlockedNames 保持一致） */
const HEADER_OVERRIDE_BLOCKED_NAMES = new Set([
  'host',
  'content-length',
  'content-type',
  'transfer-encoding',
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'proxy-connection',
  'te',
  'trailer',
  'upgrade',
  'authorization',
  'x-api-key',
  'x-goog-api-key',
  'cookie',
  'accept-encoding',
  'sec-websocket-key',
  'sec-websocket-version',
  'sec-websocket-extensions',
  'sec-websocket-protocol',
  'sec-websocket-accept',
  'session_id',
  'conversation_id',
  'x-codex-turn-state',
  'x-codex-turn-metadata',
  'chatgpt-account-id',
  'x-claude-code-session-id',
  'x-client-request-id',
  'x-grok-conv-id',
  'user-agent',
  'x-app',
  'anthropic-version',
  'anthropic-beta',
  'anthropic-dangerous-direct-browser-access',
  'originator',
  'openai-beta',
  'version'
])

/** RFC 7230 token：合法的 HTTP header 名称字符集 */
const HEADER_NAME_PATTERN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

function isValidHeaderOverrideName(name: string): boolean {
  return HEADER_NAME_PATTERN.test(name)
}

function isBlockedHeaderOverrideName(lowerName: string): boolean {
  return lowerName.startsWith('x-stainless-') || HEADER_OVERRIDE_BLOCKED_NAMES.has(lowerName)
}

/**
 * 身份指纹由网关兼容层统一生成，模板只提供不会改变 Claude/Codex 身份的通用协商头。
 */
const SAFE_HEADER_OVERRIDE_TEMPLATE = ['accept', 'accept-language']

export function getHeaderOverrideTemplate(platform: string): HeaderOverrideRow[] {
  if (!isHeaderOverridePlatform(platform)) return []
  return SAFE_HEADER_OVERRIDE_TEMPLATE.map((name) => ({ name, value: '' }))
}

/** 与后端 maxHeaderOverride* 常量保持一致 */
const HEADER_OVERRIDE_MAX_ENTRIES = 64
const HEADER_OVERRIDE_MAX_NAME_LENGTH = 200
const HEADER_OVERRIDE_MAX_VALUE_LENGTH = 8192

/** header value 不允许包含控制字符（与后端 httpguts.ValidHeaderFieldValue 对齐） */
// eslint-disable-next-line no-control-regex
const HEADER_VALUE_INVALID_PATTERN = /[\x00-\x08\x0a-\x1f\x7f]/

/** 长度限制按 UTF-8 字节计（与后端 Go len() 对齐，避免多字节值前端放行后端 400） */
const HEADER_TEXT_ENCODER = new TextEncoder()
function utf8ByteLength(value: string): number {
  return HEADER_TEXT_ENCODER.encode(value).length
}

/**
 * 校验请求头覆写行，返回首个错误的 i18n key（无错误返回 null）。
 * 名称为空但值非空 → invalidName；名称非法 → invalidName；
 * 禁止覆写 → blockedName；大小写不敏感重名 → duplicateName；
 * 值含控制字符或超长 → invalidValue；条目过多 → tooManyEntries。
 */
export function validateHeaderOverrideRows(
  rows: HeaderOverrideRow[]
): 'invalidName' | 'blockedName' | 'duplicateName' | 'invalidValue' | 'tooManyEntries' | null {
  const seen = new Set<string>()
  for (const row of rows) {
    const name = row.name.trim()
    const value = row.value.trim()
    if (!name) {
      if (value) return 'invalidName'
      continue
    }
    if (!isValidHeaderOverrideName(name) || name.length > HEADER_OVERRIDE_MAX_NAME_LENGTH) {
      return 'invalidName'
    }
    const lower = name.toLowerCase()
    if (isBlockedHeaderOverrideName(lower)) return 'blockedName'
    if (seen.has(lower)) return 'duplicateName'
    if (
      HEADER_VALUE_INVALID_PATTERN.test(value) ||
      utf8ByteLength(value) > HEADER_OVERRIDE_MAX_VALUE_LENGTH
    ) {
      return 'invalidValue'
    }
    seen.add(lower)
  }
  if (seen.size > HEADER_OVERRIDE_MAX_ENTRIES) return 'tooManyEntries'
  return null
}

/** 行数组 → credentials 存储对象（名称小写化，丢弃空行） */
export function buildHeaderOverridesObject(rows: HeaderOverrideRow[]): Record<string, string> {
  const result: Record<string, string> = {}
  for (const row of rows) {
    const name = row.name.trim().toLowerCase()
    if (!name) continue
    result[name] = row.value.trim()
  }
  return result
}

/** credentials 存储对象 → 行数组（按名称排序保证稳定展示） */
export function splitHeaderOverridesObject(record: unknown): HeaderOverrideRow[] {
  if (!record || typeof record !== 'object' || Array.isArray(record)) return []
  return Object.entries(record as Record<string, unknown>)
    .filter(([, value]) => typeof value === 'string')
    .map(([name, value]) => ({ name, value: value as string }))
    .sort((a, b) => a.name.localeCompare(b.name))
}

/**
 * 解析粘贴的 JSON 文本为请求头覆写行。
 * 仅接受扁平对象；string/number/boolean 值统一转为字符串。
 */
export function parseHeaderOverridesJson(text: string): HeaderOverrideRow[] | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    return null
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null

  const rows: HeaderOverrideRow[] = []
  for (const [rawName, rawValue] of Object.entries(parsed as Record<string, unknown>)) {
    const name = rawName.trim()
    if (!name) continue
    if (
      typeof rawValue !== 'string' &&
      typeof rawValue !== 'number' &&
      typeof rawValue !== 'boolean'
    ) {
      return null
    }
    rows.push({ name, value: String(rawValue).trim() })
  }
  return rows.sort((a, b) => a.name.localeCompare(b.name))
}

/** 请求头覆写行转换为便于迁移的 JSON；空名称占位行不写入。 */
export function serializeHeaderOverrideRows(rows: HeaderOverrideRow[]): string {
  const record: Record<string, string> = {}
  for (const row of rows) {
    const name = row.name.trim()
    if (!name) continue
    record[name] = row.value.trim()
  }
  return JSON.stringify(record, null, 2)
}

// ========== Grok 自定义转发地址 ==========

const GROK_DEFAULT_GATEWAY_HOST = 'cli-chat-proxy.grok.com'

/**
 * CLI 网关是 OAuth 凭证的默认端点；官方 API、区域 API 与第三方端点都应
 * 作为运营方显式选择回显，便于在单端点异常时切换。
 */
export function isCustomGrokBaseUrl(value: unknown): boolean {
  if (typeof value !== 'string') return false
  const trimmed = value.trim()
  if (!trimmed) return false
  let parsed: URL
  try {
    parsed = new URL(trimmed)
  } catch {
    return false
  }
  return parsed.hostname.toLowerCase() !== GROK_DEFAULT_GATEWAY_HOST
}

export interface GrokBaseUrlPreset {
  labelKey?: 'cli' | 'official'
  label?: string
  url: string
}

export const GROK_BASE_URL_PRESETS: GrokBaseUrlPreset[] = [
  { labelKey: 'cli', url: 'https://cli-chat-proxy.grok.com/v1' },
  { labelKey: 'official', url: 'https://api.x.ai/v1' },
  { label: 'us-east-1', url: 'https://us-east-1.api.x.ai/v1' },
  { label: 'us-west-2', url: 'https://us-west-2.api.x.ai/v1' },
  { label: 'eu-west-1', url: 'https://eu-west-1.api.x.ai/v1' }
]

// ========== 国产供应商（Kimi / Zhipu / DeepSeek）base_url 预设 ==========
// 与后端 service/domain_constants.go 的默认 base url 保持一致。
// 账号类型（payg 按量付费 / coding 编程套餐）决定额度监控方式；
// API 协议（chat_completions / anthropic / responses）决定转发端点与格式，
// 两者正交。同协议请求零转换直通，跨协议组合才走转换链。

export type CnAccountMode = 'payg' | 'coding'

/** 仅 deepseek 支持原生 responses；adaptive 会按入站协议选择原生端点。 */
export type CnApiProtocol = 'adaptive' | 'chat_completions' | 'anthropic' | 'responses'
export type CnNativeApiProtocol = Exclude<CnApiProtocol, 'adaptive'>

export interface CnBaseUrlPreset {
  mode: CnAccountMode
  protocol: CnApiProtocol
  /** 专有名词，不参与 i18n */
  label: string
  url: string
}

/** 各供应商按账号类型 × API 协议分档的快捷端点（点击快速填充，输入框仍可自由填写）。 */
export const CN_BASE_URL_PRESETS: Record<'kimi' | 'zhipu' | 'deepseek', CnBaseUrlPreset[]> = {
  kimi: [
    { mode: 'payg', protocol: 'chat_completions', label: 'Moonshot', url: 'https://api.moonshot.cn/v1' },
    { mode: 'payg', protocol: 'anthropic', label: 'Moonshot Anthropic', url: 'https://api.moonshot.cn/anthropic' },
    { mode: 'coding', protocol: 'chat_completions', label: 'Kimi For Coding', url: 'https://api.kimi.com/coding/v1' },
    { mode: 'coding', protocol: 'anthropic', label: 'Kimi Coding Anthropic', url: 'https://api.kimi.com/coding' }
  ],
  zhipu: [
    { mode: 'payg', protocol: 'chat_completions', label: 'GLM PaaS', url: 'https://open.bigmodel.cn/api/paas/v4' },
    { mode: 'payg', protocol: 'anthropic', label: 'GLM Anthropic', url: 'https://open.bigmodel.cn/api/anthropic' },
    { mode: 'coding', protocol: 'chat_completions', label: 'GLM Coding', url: 'https://open.bigmodel.cn/api/coding/paas/v4' },
    { mode: 'coding', protocol: 'anthropic', label: 'GLM Coding Anthropic', url: 'https://open.bigmodel.cn/api/anthropic' }
  ],
  deepseek: [
    { mode: 'payg', protocol: 'chat_completions', label: 'DeepSeek', url: 'https://api.deepseek.com' },
    { mode: 'payg', protocol: 'anthropic', label: 'DeepSeek Anthropic', url: 'https://api.deepseek.com/anthropic' },
    { mode: 'payg', protocol: 'responses', label: 'DeepSeek Responses', url: 'https://api.deepseek.com' }
  ]
}

/** 返回指定供应商 + 账号类型 + API 协议的默认 base url。 */
export function defaultCNBaseUrl(
  platform: string,
  mode: CnAccountMode,
  protocol: CnApiProtocol = 'chat_completions'
): string {
  if (protocol === 'anthropic') {
    switch (platform) {
      case 'kimi':
        return mode === 'coding' ? 'https://api.kimi.com/coding' : 'https://api.moonshot.cn/anthropic'
      case 'zhipu':
        return 'https://open.bigmodel.cn/api/anthropic'
      case 'deepseek':
        return 'https://api.deepseek.com/anthropic'
      default:
        return ''
    }
  }
  // responses 仅 deepseek：base 与 chat_completions 相同（端点路径差异由后端处理）。
  switch (platform) {
    case 'kimi':
      return mode === 'coding' ? 'https://api.kimi.com/coding/v1' : 'https://api.moonshot.cn/v1'
    case 'zhipu':
      return mode === 'coding'
        ? 'https://open.bigmodel.cn/api/coding/paas/v4'
        : 'https://open.bigmodel.cn/api/paas/v4'
    case 'deepseek':
      return 'https://api.deepseek.com'
    default:
      return ''
  }
}

/** 返回自适应模式下需要配置的原生协议及其默认端点。 */
export function defaultCNAdaptiveBaseUrls(
  platform: 'kimi' | 'zhipu' | 'deepseek',
  mode: CnAccountMode
): Record<CnNativeApiProtocol, string> {
  return {
    chat_completions: defaultCNBaseUrl(platform, mode, 'chat_completions'),
    anthropic: defaultCNBaseUrl(platform, mode, 'anthropic'),
    responses: platform === 'deepseek' ? defaultCNBaseUrl(platform, mode, 'responses') : ''
  }
}

// ===== 国产供应商用量单元格可见性（单一事实源） =====
// CNProviderQuotaCell / CNProviderBalanceCell 与 AccountUsageCell 的占位符判定
// 共用，避免多处复制条件后一处改另一处漏改。

export function cnQuotaCellVisible(platform: string, accountMode: string): boolean {
  return (platform === 'kimi' || platform === 'zhipu') && accountMode === 'coding'
}

export function cnBalanceCellVisible(platform: string, accountMode: string): boolean {
  return (platform === 'kimi' || platform === 'deepseek') && accountMode !== 'coding'
}

/**
 * 将请求头覆写写入 credentials。
 * create 模式：关闭时不写入任何字段；edit 模式：关闭时删除字段（全量替换语义）。
 */
export function applyHeaderOverride(
  credentials: Record<string, unknown>,
  enabled: boolean,
  rows: HeaderOverrideRow[],
  mode: 'create' | 'edit'
): void {
  if (enabled) {
    credentials[HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY] = true
    credentials[HEADER_OVERRIDES_CREDENTIAL_KEY] = buildHeaderOverridesObject(rows)
  } else if (mode === 'edit') {
    delete credentials[HEADER_OVERRIDE_ENABLED_CREDENTIAL_KEY]
    delete credentials[HEADER_OVERRIDES_CREDENTIAL_KEY]
  }
}
