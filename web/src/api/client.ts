/**
 * HTTP 客户端：所有接口调用的唯一出口（FRONTEND §2.2）。
 *
 * 统一处理三件横切关注点，页面代码不必重复：
 *   - **401**：清空登录态（跳转由路由守卫完成，同时记录回跳地址）；
 *   - **428**：高风险操作缺少二次验证 —— 交给注册的处理器完成验证后
 *     **自动重放一次原请求**（仅一次，防死循环，见 f-10-01 R-006）；
 *   - **错误归一**：把后端的 `{error:{code,message,details}}` 转成 ApiError，
 *     页面只判断 `code`，不解析 HTTP 状态码。
 *
 * 令牌在 HttpOnly Cookie 中，**前端不接触**（f-1-01 Q-008），因此这里没有
 * Authorization 头，只需要 `credentials: 'same-origin'`。
 */

/** 字段级错误明细，用于表单就地提示。 */
export interface FieldDetail {
  field: string
  reason: string
}

/** 后端统一错误体（API.md §3.2）。 */
interface ErrorBody {
  code: string
  message: string
  details?: FieldDetail[]
}

/** 统一成功响应体（API.md §3.1）。 */
interface SuccessBody<T> {
  data: T
  request_id: string
}

/** 分页元信息（API.md §2）。 */
export interface Pagination {
  page: number
  page_size: number
  total: number
  total_pages: number
}

/** 分页响应体：`data` 为元素数组，`pagination` 与过滤后的结果集一致。 */
interface PagedBody<T> extends SuccessBody<T[]> {
  pagination: Pagination
}

/** 业务错误。页面据此分支，`message` 可直接展示给用户。 */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: FieldDetail[]
  readonly requestId: string
  /** 附加数据。目前只有 428 会携带（验证方式与 challenge）。 */
  readonly data: unknown

  constructor(
    status: number,
    code: string,
    message: string,
    requestId: string,
    details: FieldDetail[] = [],
    data?: unknown,
  ) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
    this.data = data
  }
}

/** 一种可用的验证方式，由后端按用户实际绑定情况下发。 */
export interface RiskMethod {
  method: string
  label: string
}

/** 428 响应携带的验证要求（f-10-01 §5.2）。 */
export interface RiskRequiredData {
  action: string
  challenge_id: string
  methods: RiskMethod[]
  expires_at: string
}

/** 重放原请求时携带一次性许可的请求头。 */
export const GRANT_HEADER = 'X-Risk-Grant'

/** 网络层失败（断网、响应非 JSON）。与业务错误区分开，提示文案也不同。 */
export class NetworkError extends Error {
  constructor(cause: unknown) {
    super('网络异常，请检查连接后重试')
    this.name = 'NetworkError'
    this.cause = cause
  }
}

type UnauthorizedHandler = () => void

/** 验证处理器：展示验证框、等待用户完成，成功时返回一次性许可。 */
type RiskVerificationHandler = (required: RiskRequiredData) => Promise<string | null>

let onUnauthorized: UnauthorizedHandler | undefined
let onRiskVerification: RiskVerificationHandler | undefined

/** 注册 401 处理器。由应用启动时注入，避免请求层依赖路由与状态库。 */
export function setUnauthorizedHandler(handler: UnauthorizedHandler): void {
  onUnauthorized = handler
}

/**
 * 注册高风险二次验证处理器（f-10-01）。
 *
 * 处理器返回许可令牌表示验证通过，请求层据此重放原请求；返回 null 表示
 * 用户取消，原请求以 428 错误结束。
 */
export function setRiskVerificationHandler(handler: RiskVerificationHandler): void {
  onRiskVerification = handler
}

interface RequestOptions {
  method?: string
  body?: unknown
  query?: Record<string, string | number | undefined>
  /** 一次性许可，仅在验证通过后重放原请求时携带。 */
  grant?: string
}

function buildUrl(path: string, query?: RequestOptions['query']): string {
  if (!query) return path
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== '') params.set(key, String(value))
  }
  const qs = params.toString()
  return qs ? `${path}?${qs}` : path
}

/**
 * 底层请求：发送请求并把非 2xx 响应统一转成 ApiError。
 *
 * 返回完整响应体而非只返回 `data`——分页接口需要读取 `pagination`。
 */
async function rawRequest<B>(path: string, options: RequestOptions = {}): Promise<B> {
  const { method = 'GET', body, query, grant } = options

  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  // 许可走请求头而非 Cookie：它只对紧接着的这一次重放有意义，不应被浏览器
  // 自动附加到后续所有请求上（那会把「一次性」变成「一段时间内全程有效」）。
  if (grant) headers[GRANT_HEADER] = grant

  let response: Response
  try {
    response = await fetch(buildUrl(path, query), {
      method,
      headers: Object.keys(headers).length > 0 ? headers : undefined,
      body: body === undefined ? undefined : JSON.stringify(body),
      // 会话凭据经 Cookie 传递，必须显式带上。
      credentials: 'same-origin',
    })
  } catch (error) {
    throw new NetworkError(error)
  }

  if (response.status === 204) return undefined as B

  let payload: unknown
  try {
    payload = await response.json()
  } catch {
    throw new NetworkError(new Error(`响应不是合法 JSON: status=${response.status}`))
  }

  if (response.ok) return payload as B

  const body_ = payload as { error?: ErrorBody; data?: unknown; request_id?: string }
  const error = new ApiError(
    response.status,
    body_.error?.code ?? 'INTERNAL_ERROR',
    body_.error?.message ?? '请求失败',
    body_.request_id ?? '',
    body_.error?.details ?? [],
    body_.data,
  )

  // 401 在任何入口都要触发登出：会话可能在任意时刻被撤销或过期。
  if (error.status === 401) onUnauthorized?.()

  throw error
}

/** 发起请求并返回 `data`；遇到 428 时完成二次验证后重放一次。 */
async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  try {
    const payload = await rawRequest<SuccessBody<T>>(path, options)
    return payload?.data as T
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 428) throw error

    // 已经因为 428 重放过一次，仍然被拒：说明许可未被识别（过期、已被消费、
    // 会话不匹配）。此时**直接报错，不再弹框**——继续弹会让用户在「验证成功
    // 却毫无反应」之间循环，而且退不出去（f-10-01 Q-010）。
    if (options.grant) {
      throw new ApiError(
        error.status,
        error.code,
        '验证状态已失效，请重试刚才的操作',
        error.requestId,
      )
    }
    if (!onRiskVerification) throw error

    const grant = await onRiskVerification(error.data as RiskRequiredData)
    if (!grant) throw error

    // 重放**原样**的请求（方法、路径、查询、请求体完全一致），只多一个许可头。
    const payload = await rawRequest<SuccessBody<T>>(path, { ...options, grant })
    return payload?.data as T
  }
}

/** 发起 GET 请求。 */
export function get<T>(path: string, query?: RequestOptions['query']): Promise<T> {
  return request<T>(path, { query })
}

/** 发起 POST 请求。 */
export function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, { method: 'POST', body })
}

/** 发起 PUT 请求。 */
export function put<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, { method: 'PUT', body })
}

/** 发起 PATCH 请求。 */
export function patch<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, { method: 'PATCH', body })
}

/** 发起 DELETE 请求。 */
export function del<T>(path: string): Promise<T> {
  return request<T>(path, { method: 'DELETE' })
}

/**
 * 发起 GET 并返回分页数据。
 *
 * 注意泛型参数是**元素类型**：调用方写 `getPaged<VmSummary>(...)`，
 * 而不是 `VmVmSummary[]>`。
 */
export async function getPaged<T>(
  path: string,
  query?: RequestOptions['query'],
): Promise<{ items: T[]; pagination: Pagination }> {
  const payload = await rawRequest<PagedBody<T>>(path, { query })
  return { items: payload.data, pagination: payload.pagination }
}
