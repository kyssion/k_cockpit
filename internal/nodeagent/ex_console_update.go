package nodeagent

import (
	"context"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// vmConsoleUpdate 实现 vm.console.update（F-2-08 / F-2-09）。
//
// 参数：
//   - protocol: vnc / spice（缺省 vnc）
//   - enabled:  开启或关闭该协议的控制台
//   - listen:   监听地址（对外暴露 → 0.0.0.0，默认 127.0.0.1）
//   - port:     固定端口；0 表示交给 libvirt 自动分配
//   - password: 新密码（空表示保持原密码）
//
// 返回的 restart_required 表示改动已写入持久配置，但运行中的域要重启才生效：
// libvirt 不重启域就无法改变 graphics 的监听地址。
func vmConsoleUpdate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	protocol := strParam(op.Params, "protocol")
	if protocol != "spice" {
		protocol = "vnc"
	}
	enabled := boolParam(op.Params, "enabled")
	listen := strParam(op.Params, "listen")
	if listen == "" {
		listen = "127.0.0.1"
	}
	port := 0
	if v, ok := op.Params["port"].(float64); ok {
		port = int(v)
	}
	password := strParam(op.Params, "password")

	report(Stage{Key: "read", Name: "读取域定义", Index: 1, Total: 2})
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}

	newXML, changed := applyConsoleGraphics(xmlText, protocol, enabled, listen, port, password)
	if !changed {
		// 幂等：重复提交同样的配置不应触发一次多余的 define。
		msg := "控制台配置没有变化"
		if enabled {
			msg = "控制台配置已是目标状态"
		}
		return &agent.Result{Success: true, Message: msg}, nil
	}

	report(Stage{Key: "define", Name: "写入域定义", Index: 2, Total: 2})
	if err := defineXML(ctx, newXML); err != nil {
		// libvirt 拒绝的常见原因是编译期没带 SPICE 支持，或端口被占用。
		// 把 stderr 带回去，用户才知道是"不支持"还是"端口冲突"。
		return failResult("libvirt 拒绝了控制台配置: " + err.Error()), nil
	}

	running, _ := vmRunning(ctx, op.Target)
	msg := "控制台配置已写入"
	if running {
		msg += "；运行中的虚拟机需重启后生效"
	}
	return &agent.Result{
		Success: true,
		Message: msg,
		Data: map[string]any{
			agent.ConsoleUpdatePortKey:            port,
			agent.ConsoleUpdateRestartRequiredKey: running,
		},
	}, nil
}
