package ops

import (
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/realtime"
)

func TestVisibleToFiltersTenantEvents(t *testing.T) {
	admin := authz.Viewer{IsAdmin: true}
	tenant := authz.Viewer{UserID: 7}
	other := authz.Viewer{UserID: 8}

	cases := []struct {
		name  string
		event realtime.Event
		admin bool
		self  bool
		other bool
	}{
		{
			name:  "有归属的任务事件",
			event: realtime.Event{Kind: realtime.KindTask, OwnerID: 7, TaskID: 12},
			admin: true, self: true, other: false,
		},
		{
			// 无归属的节点级事件只归管理员：让租户知道"存在这么一个任务"
			// 本身就是信息，而且他点进去也看不到内容。
			name:  "无归属的节点级事件",
			event: realtime.Event{Kind: realtime.KindTask, TaskID: 13},
			admin: true, self: false, other: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := visibleTo(admin, tc.event); got != tc.admin {
				t.Errorf("管理员可见 = %v, 期望 %v", got, tc.admin)
			}
			if got := visibleTo(tenant, tc.event); got != tc.self {
				t.Errorf("归属用户可见 = %v, 期望 %v", got, tc.self)
			}
			if got := visibleTo(other, tc.event); got != tc.other {
				t.Errorf("其他租户可见 = %v, 期望 %v", got, tc.other)
			}
		})
	}
}

func TestWriteSSE(t *testing.T) {
	var buf strings.Builder
	if !writeSSE(&buf, "task", map[string]any{"id": 12}) {
		t.Fatal("写入成功应返回 true")
	}
	want := "event: task\ndata: {\"id\":12}\n\n"
	if buf.String() != want {
		t.Errorf("SSE 帧 = %q, 期望 %q", buf.String(), want)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestWriteSSEReportsClosedWriter(t *testing.T) {
	if writeSSE(failingWriter{}, "task", map[string]any{"id": 1}) {
		t.Error("客户端断开时应返回 false，让写协程退出")
	}
}
