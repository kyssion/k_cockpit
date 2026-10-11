// console.go：控制台相关——截帧（qemu monitor screendump）与 VNC 流。
package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// vmConsoleFrame 实现 vm.console.frame：一帧控制台画面。
//
// 走 `virsh screenshot`（libvirt 10 在本环境直接输出 PNG），文件由 libvirt
// 以 root 落盘——qemu-monitor-command 的 screendump 会被 qemu 用户的写
// 权限挡住（真机实测无输出文件）。
func vmConsoleFrame(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "screendump", Name: "截取画面", Index: 1, Total: 2})
	tmp := filepath.Join(os.TempDir(), "kc-frame-"+op.Target+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".ppm")
	if _, err := sys.Run(ctx, "virsh", "screenshot", op.Target, tmp); err != nil {
		return failResult("截帧不可用（虚拟机可能未运行或无显示设备）: " + err.Error()), nil
	}
	b, err := os.ReadFile(tmp)
	_ = os.Remove(tmp)
	if err != nil || len(b) == 0 {
		return failResult("截帧输出为空"), nil
	}

	report(Stage{Key: "encode", Name: "回传画面", Index: 2, Total: 2})
	mime := "image/png"
	if len(b) > 4 && string(b[1:4]) != "PNG" {
		mime = "image/x-portable-pixmap" // 旧 libvirt 的 PPM 输出，前端按需转换
	}
	w, h := pngSize(b)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.FrameDataKey: agent.ConsoleFrame{MIME: mime, Data: b, Width: w, Height: h},
	}}, nil
}

// pngSize 从 PNG 头读宽高（IHDR 固定在 16..24 字节），失败返回 0。
func pngSize(b []byte) (int, int) {
	if len(b) < 24 {
		return 0, 0
	}
	w := int(b[16])<<24 | int(b[17])<<16 | int(b[18])<<8 | int(b[19])
	h := int(b[20])<<24 | int(b[21])<<16 | int(b[22])<<8 | int(b[23])
	return w, h
}

const timeSecond = time.Second
