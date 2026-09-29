package api

import (
	"context"
	"log"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// SecurityHeaders 为所有响应补齐全局安全响应头。
//
// 此前 nosniff 只在下载类接口（控制台连接文件、抓包、我的存储）逐个手设，
// 而 X-Frame-Options / CSP / Referrer-Policy 全仓缺失——响应头这种「每个
// 响应都该有」的东西放在中间件里是唯一不会漏的位置。
//
// CSP 只声明 frame-ancestors：完整 default-src 需要处理 SPA 的内联主题
// 脚本与 noVNC 的 WebSocket，贸然收紧会把界面挡在自己门外，等前端构建
// 支持内容哈希后再补。
func SecurityHeaders() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// 注意逐条直接调用而不是先取出 Header 再 Set：Header 是值字段，
		// 取出来的是副本，写在副本上等于没写。
		c.Response.Header.Set("X-Content-Type-Options", "nosniff")
		c.Response.Header.Set("X-Frame-Options", "DENY")
		c.Response.Header.Set("Content-Security-Policy", "frame-ancestors 'none'")
		// no-referrer 而不是默认的 strict-origin：面板地址可能携带内网
		// 信息，外链跳转时没有必要把它带出去。
		c.Response.Header.Set("Referrer-Policy", "no-referrer")
		// API 响应一律不缓存（F-10-04）：接口返回的是**此刻的状态**
		//（虚拟机列表、任务进度），被中间层缓存后用户会看到过去时。
		// 静态资源不走这条：SPA 的带哈希产物正该被缓存。
		path := string(c.Request.URI().Path())
		if strings.HasPrefix(path, "/api/") || path == "/health" {
			c.Response.Header.Set("Cache-Control", "no-store")
		}
		c.Next(ctx)
	}
}

// 请求行各段的长度上限（F-10-03 的"超长查询参数"防护）。
//
// 取值依据：正常业务的查询串（分页 + 过滤 + 排序）远用不到 2 KB；而
// 攻击者借超长查询串做的事（填充日志、撑爆解析、绕过 WAF 前缀匹配）
// 需要的是数量级。给一个宽松十倍的余量，正常请求永远碰不到它。
const (
	maxPathLen  = 1024
	maxQueryLen = 2048
)

// 携带请求体且声明了 Content-Type 时允许的类型。
//
// 只拦「声明了却不在清单内」的请求：分片上传发的是**不带 Content-Type**
// 的裸二进制体（见前端 putRaw），无体 GET/DELETE 与手测 curl 也不受影响。
var allowedContentTypes = []string{
	"application/json",
	"multipart/form-data",               // 分片上传之外的表单场景
	"application/x-www-form-urlencoded", // curl -d 未显式指定时的默认值
	"text/plain",
}

// scannerProbes 是已知扫描器路径前缀/文件名（小写）。
//
// 这些路径在本面板的路由表里不存在，正常用户永远不会访问到；命中只说明
// 是自动化扫描。拦截它们不是为了「隐藏」，而是让扫描流量在进业务逻辑
// （以及可能触发 404 日志告警）之前就终止。
var scannerProbes = []string{
	"/.env", "/.git", "/.svn", "/.htaccess", "/.aws", "/.ds_store",
	"/wp-admin", "/wp-login.php", "/wp-json", "/xmlrpc.php",
	"/phpmyadmin", "/pma", "/administrator", "/actuator", "/config.json",
}

// InputFilter 是输入侧防护（请求过滤）。
//
// 拦三类请求：路径或查询串里的控制字符与空字节、解码后仍指向上级目录的
// 路径穿越、已知扫描器探测路径；此外，携带请求体但 Content-Type 不在
// 白名单内的请求直接拒绝。
//
// enabled 为 nil 时视为恒开；传入函数以便从系统设置读取开关——每次请求
// 都现读，改设置立即生效。被拦的请求返回 403 而不是 404：404 会让扫描
// 方以为「路径存在但碰巧没找到」，从而换着花样继续试。
func InputFilter(enabled func() bool) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if enabled != nil && !enabled() {
			c.Next(ctx)
			return
		}

		// 穿越与编码攻击看**原始请求行**：框架的 URI().Path() 已经做了解码
		// 与归一化，../ 段在那里被消掉，等中间件看到时穿越已经不可见——
		// 路由层通常不会再匹配到目标，但那表现为一个安静的 404，而不是
		// 一次被记录的拦截。
		raw := string(c.Request.URI().RequestURI())
		if reason := rejectReason(raw); reason != "" {
			log.Printf("[guard] 拒绝请求 ip=%s uri=%s reason=%s", c.ClientIP(), raw, reason)
			Fail(c, PermissionDenied("请求被拒绝"))
			// Hertz 的 Next 是循环实现：只 return 不阻断链，后续 handler
			// 仍会执行并覆盖状态码（与 auth 中间件同一写法）。
			c.Abort()
			return
		}
		// 长度上限单独拦（F-10-03）：理由不混进 rejectReason——那里判的是
		// 内容恶意，这里判的是尺寸异常，日志里要能分清是哪一类。
		if p, q := string(c.Request.URI().Path()), string(c.Request.URI().QueryString()); len(p) > maxPathLen || len(q) > maxQueryLen {
			log.Printf("[guard] 拒绝请求 ip=%s path_len=%d query_len=%d", c.ClientIP(), len(p), len(q))
			Fail(c, PermissionDenied("请求路径或查询参数过长"))
			c.Abort()
			return
		}

		if len(c.Request.Body()) > 0 {
			ct := strings.TrimSpace(string(c.Request.Header.ContentType()))
			// 未声明 Content-Type 的体放行：分片上传（putRaw）就是这样
			// 发裸二进制的，拦它会打断「我的存储」与两条导入链路。
			if ct != "" {
				ct = strings.ToLower(strings.SplitN(ct, ";", 2)[0])
				allowed := false
				for _, want := range allowedContentTypes {
					if ct == want {
						allowed = true
						break
					}
				}
				if !allowed {
					log.Printf("[guard] 拒绝请求 ip=%s path=%s content_type=%s",
						c.ClientIP(), string(c.Request.URI().Path()), ct)
					Fail(c, PermissionDenied("不支持的请求内容类型"))
					c.Abort()
					return
				}
			}
		}

		// 扫描器探测看解码后的路径：探测目标（/.env 等）本身不含编码。
		if probe, hit := scannerProbeOf(string(c.Request.URI().Path())); hit {
			log.Printf("[guard] 拒绝请求 ip=%s path=%s probe=%s",
				c.ClientIP(), string(c.Request.URI().Path()), probe)
			Fail(c, PermissionDenied("请求被拒绝"))
			return
		}

		c.Next(ctx)
	}
}

// rejectReason 检查原始请求行，返回拒绝原因；空串表示放行。
func rejectReason(raw string) string {
	// 原始请求行里的控制字符到不了这里（HTTP 解析器先拒绝），要检查的是
	// **解码后**的值：%00 这类编码只有在解码后才现形。
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "malformed_escape"
	}
	for i := 0; i < len(decoded); i++ {
		if decoded[i] < 0x20 || decoded[i] == 0x7f {
			return "control_character"
		}
	}
	// 穿越判定同时覆盖正反斜杠：Windows 路径分隔符在 URL 里不合法，
	// 出现它本就只有穿越一种解释。
	if strings.Contains(decoded, "../") || strings.Contains(decoded, "..\\") {
		return "path_traversal"
	}
	return ""
}

// scannerProbeOf 返回命中的扫描器探测路径。
func scannerProbeOf(path string) (string, bool) {
	lower := strings.ToLower(path)
	for _, probe := range scannerProbes {
		if lower == probe || strings.HasPrefix(lower, probe+"/") {
			return probe, true
		}
	}
	return "", false
}
