// Package api 提供 HTTP 层的通用约定：统一响应、错误码与中间件。
//
// 本包是 docs/03-api/API.md 第 1、3 节的唯一实现。所有接口都必须经由本包
// 返回响应，以保证响应结构、错误码与 request_id 在全项目保持一致。
package api

import (
	"net/http"
)

// 稳定的机器可读错误码。前端据此分支，**新增前必须先登记到
// docs/03-api/API.md §3.3**，禁止临时编造。
const (
	CodeInvalidParameter   = "INVALID_PARAMETER"
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodePermissionDenied   = "PERMISSION_DENIED"
	CodeResourceNotFound   = "RESOURCE_NOT_FOUND"
	CodeResourceConflict   = "RESOURCE_CONFLICT"
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeRiskVerification   = "RISK_VERIFICATION_REQUIRED"
	CodeRateLimited        = "RATE_LIMITED"
	CodeInternal           = "INTERNAL_ERROR"
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// Detail 是字段级错误明细，供前端就地提示到具体输入项。
type Detail struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Error 是携带 HTTP 状态码与稳定错误码的业务错误。
//
// 它既是 error，也是对外响应的一部分：Code 一经发布不可改动（前端依赖它），
// Message 面向用户、可调整文案，但**不得包含堆栈、SQL、路径等内部细节**。
type Error struct {
	Status  int
	Code    string
	Message string
	Details []Detail
}

func (e *Error) Error() string {
	return e.Code + ": " + e.Message
}

// InvalidParameter 参数不合法（400）：类型、格式、范围、长度不满足要求。
func InvalidParameter(message string, details ...Detail) *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidParameter, Message: message, Details: details}
}

// Unauthenticated 未认证或凭据失效（401），前端应引导重新登录。
func Unauthenticated(message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: CodeUnauthenticated, Message: message}
}

// PermissionDenied 已认证但无权限（403）。
func PermissionDenied(message string) *Error {
	return &Error{Status: http.StatusForbidden, Code: CodePermissionDenied, Message: message}
}

// NotFound 资源不存在（404）。**越权访问他人的资源也返回此错误**，
// 避免通过 403 与 404 的差异探测资源是否存在（见 f-1-06）。
func NotFound(message string) *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeResourceNotFound, Message: message}
}

// Conflict 状态冲突（409）：如重复创建、当前状态不允许该操作。
func Conflict(message string) *Error {
	return &Error{Status: http.StatusConflict, Code: CodeResourceConflict, Message: message}
}

// ValidationFailed 语义校验失败（422）：格式合法但业务上不可接受。
func ValidationFailed(message string, details ...Detail) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed, Message: message, Details: details}
}

// RiskVerificationRequired 高风险操作需先完成二次验证（428）。
// 响应体还需携带可用验证方式与 challenge_id，由 f-10-01 规定。
func RiskVerificationRequired(message string) *Error {
	return &Error{Status: http.StatusPreconditionRequired, Code: CodeRiskVerification, Message: message}
}

// RateLimited 触发限流（429）。
func RateLimited(message string) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: CodeRateLimited, Message: message}
}

// Unavailable 依赖不可用（503）：如节点离线、上游不可达。
func Unavailable(message string) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeServiceUnavailable, Message: message}
}

// Internal 服务内部错误（500）。
//
// **刻意不接收自定义消息**：内部错误的细节只进日志，不进响应，
// 从签名上杜绝「把 err 直接交给用户」这类泄漏。
func Internal() *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "服务内部错误，请稍后重试"}
}
