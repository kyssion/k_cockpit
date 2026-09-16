package agent

import (
	"context"
	"errors"
	"io"
)

// ErrStreamUnsupported 表示该实现不支持流式转发。
//
// 它是一个**哨兵错误**而不是内部故障：调用方据此给出「当前节点不支持
// 控制台」这类明确提示，而不是把它当成异常。区分这两者是必要的——
// 「不支持」有确定的处理方式，「故障」需要重试。
var ErrStreamUnsupported = errors.New("agent: stream transport not supported")

// StreamKind 是流的类型。
type StreamKind string

// 流类型。
const (
	// StreamVNC 是 VNC 控制台流量。
	StreamVNC StreamKind = "vnc"
)

// Stream 是一条与节点之间的双向字节流。
//
// 它是控制台流量的承载抽象：控制面把浏览器侧的 WebSocket 与这里的流对接，
// 两端都不感知对方的存在。**流量不落盘、不进日志**（f-2-08 R-013）——
// 控制台里可能有用户输入的凭据，写进日志等于把它们扩散到日志系统。
type Stream interface {
	io.ReadWriteCloser
}

// StreamOpener 是支持流式转发的 Client 的能力。
//
// 单独定义而不并入 Client：并非所有实现都具备流式转发，把它们塞进主接口
// 会迫使每个实现都写一个「不支持」的空方法——那种方法没有任何信息量，
// 却会让「谁支持什么」变得模糊。
type StreamOpener interface {
	// OpenStream 打开一条指向指定资源的流。
	//
	// 返回 ErrStreamUnsupported 表示该实现不支持；其他错误表示打开失败。
	OpenStream(ctx context.Context, kind StreamKind, nodeID int64, target string) (Stream, error)
}
