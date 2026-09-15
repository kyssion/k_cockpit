package handler

import (
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
)

// pathID 解析路径中的数字 ID。
//
// 非法 ID 一律返回 400 而不是 404：前者说明请求本身有问题（拼错了），
// 后者会让调用方以为「资源不存在」而去别处排查。
func pathID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, api.InvalidParameter("ID 不合法")
	}
	return id, nil
}

// namedPathID 解析带字段名的路径 ID，用于错误提示更精确的场景。
func namedPathID(c *app.RequestContext, name, label string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, api.InvalidParameter(label + "不合法")
	}
	return id, nil
}

// queryInt 读取整数查询参数；缺失或非法时返回 0，由调用方决定默认值。
//
// 查询参数非法**不报错**而是回落默认值：分页参数写错时，返回第一页
// 比返回一个错误页更有用——用户至少能看到数据。
func queryInt(c *app.RequestContext, name string) int {
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
