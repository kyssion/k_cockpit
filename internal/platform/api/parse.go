package api

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// 请求解析与下载头：被全部 handler 域共享，因此放在 api 包——
// handler 按域拆分（ADR-0010）后，这些助手不属于任何一个域。

// PathID 解析路径中的数字 ID。
//
// 非法 ID 一律返回 400 而不是 404：前者说明请求本身有问题（拼错了），
// 后者会让调用方以为「资源不存在」而去别处排查。
func PathID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, InvalidParameter("ID 不合法")
	}
	return id, nil
}

// NamedPathID 解析带字段名的路径 ID，用于错误提示更精确的场景。
func NamedPathID(c *app.RequestContext, name, label string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, InvalidParameter(label + "不合法")
	}
	return id, nil
}

// QueryInt 读取整数查询参数；缺失或非法时返回 0，由调用方决定默认值。
//
// 查询参数非法**不报错**而是回落默认值：分页参数写错时，返回第一页
// 比返回一个错误页更有用——用户至少能看到数据。
func QueryInt(c *app.RequestContext, name string) int {
	raw := c.Query(name)
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

// ContentDisposition 生成下载响应的文件头。
//
// 文件名必须由服务端决定，且用 RFC 5987 的形式处理非 ASCII：
// `filename` 给只认 ASCII 的老客户端兜底，`filename*` 带编码给现代客户端。
// 兜底名替换非法字符而不是丢弃，是为了保留长度与大致形状——用户至少
// 能分辨是哪个文件。
func ContentDisposition(name string) string {
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii.WriteByte('_')
			continue
		}
		ascii.WriteRune(r)
	}
	fallback := ascii.String()
	if fallback == "" {
		fallback = "download"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` +
		url.PathEscape(name)
}

// PageParams 解析分页参数并回落默认值（page=1 / page_size=20）。
//
// 与 QueryInt 同一取舍：写错的分页参数回落到第一页，而不是把用户挡在
// 一个错误页上。
func PageParams(c *app.RequestContext) (int, int) {
	page, pageSize := QueryInt(c, "page"), QueryInt(c, "page_size")
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	return page, pageSize
}
