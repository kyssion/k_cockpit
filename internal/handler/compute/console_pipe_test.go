package compute

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hertz-contrib/websocket"

	"k_cockpit/internal/agent"
)

// pipeConsole 的转发协程跑在 HTTP handler 的 goroutine 之外，api.Recover()
// 兜不住它们——所以这里必须自己验证「panic 会被就地兜住，且主协程能返回」。
// 全部用例都不依赖真实网络：conn 与 stream 都是测试替身。

// pipeReturnTimeout 是单个用例等待 pipeConsole 返回的上限。
//
// 取 3 秒：转发逻辑本身没有任何等待，正常情况下应当立即返回；这个超时只
// 用来把「协程卡死」变成一条明确的失败信息，而不是让整个测试挂到 CI 超时。
const pipeReturnTimeout = 3 * time.Second

// fakeConsoleConn 是 consoleConn 的测试替身。
//
// readFn / writeFn 为 nil 时走各自的默认行为：ReadMessage 阻塞到 Close 之后
// 返回 io.EOF，WriteMessage 直接成功。这个「阻塞到 Close」的默认值是必需的
// ——另一个转发协程总要有个地方待着，否则它会在测试里空转。
type fakeConsoleConn struct {
	readFn  func() (int, []byte, error)
	writeFn func(int, []byte) error
	closed  chan struct{}
	once    sync.Once
}

func newFakeConsoleConn() *fakeConsoleConn {
	return &fakeConsoleConn{closed: make(chan struct{})}
}

func (c *fakeConsoleConn) ReadMessage() (int, []byte, error) {
	if c.readFn != nil {
		return c.readFn()
	}
	<-c.closed
	return 0, nil, io.EOF
}

func (c *fakeConsoleConn) WriteMessage(messageType int, data []byte) error {
	if c.writeFn != nil {
		return c.writeFn(messageType, data)
	}
	return nil
}

func (c *fakeConsoleConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeConsoleConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// fakeStream 是 agent.Stream（io.ReadWriteCloser）的测试替身。
type fakeStream struct {
	readFn  func(p []byte) (int, error)
	writeFn func(p []byte) (int, error)
	closed  chan struct{}
	once    sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{closed: make(chan struct{})}
}

func (s *fakeStream) Read(p []byte) (int, error) {
	if s.readFn != nil {
		return s.readFn(p)
	}
	<-s.closed
	return 0, io.EOF
}

func (s *fakeStream) Write(p []byte) (int, error) {
	if s.writeFn != nil {
		return s.writeFn(p)
	}
	return len(p), nil
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeStream) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// 编译期确认测试替身满足被测函数的入参类型。
var (
	_ consoleConn  = (*fakeConsoleConn)(nil)
	_ agent.Stream = (*fakeStream)(nil)
)

// TestConsoleLease_ReleasesExactlyOnce 锁定 R-007/R-014 的释放语义：
// 无论三条来路（升级失败 / 连接结束 / 回调未调度）怎样竞争，会话都只释放一次。
//
// 这条不变量是那个「active_sessions 恒为 0」缺陷的另一半——释放被提前触发时
// 计数不会报错，只会安静地失真，所以只能靠测试把它钉住。
func TestConsoleLease_ReleasesExactlyOnce(t *testing.T) {
	var calls int
	lease := &consoleLease{
		vmID:      3,
		sessionID: "cs-3-test",
		releaseFn: func() { calls++ },
	}

	if !lease.release("连接结束") {
		t.Fatal("首次释放应当真正执行")
	}
	if lease.release("升级失败") {
		t.Error("第二次释放不应当再执行")
	}
	if lease.release("回调未被调度") {
		t.Error("第三次释放不应当再执行")
	}
	if calls != 1 {
		t.Fatalf("释放回调被调用了 %d 次，应当恰好 1 次", calls)
	}
}

// TestConsoleLease_ConcurrentRelease 覆盖并发释放：三条来路可能同时发生
// （例如升级失败的同时兜底超时到点）。
func TestConsoleLease_ConcurrentRelease(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	lease := &consoleLease{
		vmID:      3,
		sessionID: "cs-3-concurrent",
		releaseFn: func() {
			mu.Lock()
			calls++
			mu.Unlock()
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			lease.release(fmt.Sprintf("并发路径 %d", n))
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("并发释放下回调被调用了 %d 次，应当恰好 1 次", calls)
	}
}

// TestPipeConsole_ReturnsOnStreamError 复现实机日志里的那一行：
//
//	节点→浏览器 结束: stream.Read err=stream: 已关闭 (n=0)
//
// 也就是 agent 流在转发开始时就已经关闭。pipeConsole 必须立刻返回（不能
// 让浏览器侧协程把整个连接拖住），并且不向外抛 panic——用户看到的是黑屏，
// 而不是控制面被打挂。
func TestPipeConsole_ReturnsOnStreamError(t *testing.T) {
	conn := newFakeConsoleConn()
	stream := newFakeStream()
	stream.readFn = func([]byte) (int, error) {
		return 0, errors.New("stream: 已关闭")
	}

	runPipe(t, conn, stream)

	if !conn.isClosed() {
		t.Error("agent 流已关闭时，WebSocket 连接也应被关闭")
	}
}

// runPipe 在独立的 goroutine 里跑一次 pipeConsole，并断言它**不向外抛 panic**
// 且**能返回**。返回后 conn 与 stream 都应已被关闭（R-007 的释放语义）。
func runPipe(t *testing.T, conn consoleConn, stream agent.Stream) {
	t.Helper()

	finished := make(chan any, 1)
	go func() {
		// 这里刻意不做 recover 之外的任何处理：本用例的目的就是确认
		// pipeConsole 自己兜住了内部 panic，而不是靠测试帮忙兜。
		defer func() { finished <- recover() }()
		pipeConsole(conn, stream)
	}()

	select {
	case r := <-finished:
		if r != nil {
			t.Fatalf("pipeConsole 向外抛出了 panic: %v", r)
		}
	case <-time.After(pipeReturnTimeout):
		t.Fatalf("pipeConsole 在 %s 内未返回，转发协程可能已卡死", pipeReturnTimeout)
	}
}

// TestPipeConsole_RecoversBrowserReadPanic 复现线上那个崩溃：
// 浏览器关标签页／刷新后，hijack 连接被对端关闭，hertz-contrib/websocket
// 在 ReadMessage 内部对 boss 已失效的底层连接解引用并 panic（conn.go:376）。
// 修复前这个 panic 直接终结控制面进程。
func TestPipeConsole_RecoversBrowserReadPanic(t *testing.T) {
	conn := newFakeConsoleConn()
	conn.readFn = func() (int, []byte, error) {
		panic("runtime error: invalid memory address or nil pointer dereference")
	}
	stream := newFakeStream()

	runPipe(t, conn, stream)

	if !stream.isClosed() {
		t.Error("pipeConsole 返回后 agent 流未关闭（会话/缓冲会泄漏）")
	}
}

// TestPipeConsole_RecoversStreamReadPanic 覆盖节点侧读取 panic：
// 节点进程意外退出时，agent 流可能在 Read 里 panic。
func TestPipeConsole_RecoversStreamReadPanic(t *testing.T) {
	conn := newFakeConsoleConn()
	stream := newFakeStream()
	stream.readFn = func([]byte) (int, error) {
		panic("stream read: unexpected nil reader")
	}

	runPipe(t, conn, stream)

	if !conn.isClosed() {
		t.Error("pipeConsole 返回后 WebSocket 连接未关闭")
	}
}

// TestPipeConsole_RecoversWriteMessagePanic 覆盖向浏览器写帧时的 panic：
// 连接已被对端关闭后，WriteMessage 同样可能对 nil 底层连接解引用。
func TestPipeConsole_RecoversWriteMessagePanic(t *testing.T) {
	conn := newFakeConsoleConn()
	conn.writeFn = func(int, []byte) error {
		panic("write: use of closed hijacked connection")
	}
	stream := newFakeStream()
	stream.readFn = func(p []byte) (int, error) {
		return copy(p, "frame"), nil
	}

	runPipe(t, conn, stream)
}

// TestPipeConsole_RecoversStreamWritePanic 覆盖向节点转发时 stream.Write 的 panic。
func TestPipeConsole_RecoversStreamWritePanic(t *testing.T) {
	conn := newFakeConsoleConn()
	conn.readFn = func() (int, []byte, error) {
		return websocket.BinaryMessage, []byte("key-event"), nil
	}
	stream := newFakeStream()
	stream.writeFn = func([]byte) (int, error) {
		panic("stream write after peer gone")
	}

	runPipe(t, conn, stream)
}

// TestPipeConsole_ReturnsOnStreamEOF 是正常路径：节点侧流结束 → 转发返回 →
// 两侧都被关闭。修复不得改变这个既有语义。
func TestPipeConsole_ReturnsOnStreamEOF(t *testing.T) {
	conn := newFakeConsoleConn()
	stream := newFakeStream()
	stream.readFn = func([]byte) (int, error) {
		return 0, io.EOF
	}

	runPipe(t, conn, stream)

	if !conn.isClosed() {
		t.Error("流结束后 WebSocket 连接未关闭")
	}
	if !stream.isClosed() {
		t.Error("流结束后 agent 流未关闭")
	}
}

// TestPipeConsole_SkipsTextFrames 确认只有二进制帧被转发到节点：
// VNC 是二进制协议，混进文本帧会让节点侧的解析错位。
func TestPipeConsole_SkipsTextFrames(t *testing.T) {
	var (
		mu      sync.Mutex
		forward []byte
	)
	conn := newFakeConsoleConn()
	var calls int
	conn.readFn = func() (int, []byte, error) {
		calls++
		if calls == 1 {
			return websocket.TextMessage, []byte("not-a-vnc-frame"), nil
		}
		// 第二次读取直接结束，让协程收敛。
		return 0, nil, io.EOF
	}
	stream := newFakeStream()
	stream.writeFn = func(p []byte) (int, error) {
		mu.Lock()
		forward = append(forward, p...)
		mu.Unlock()
		return len(p), nil
	}

	runPipe(t, conn, stream)

	mu.Lock()
	defer mu.Unlock()
	if len(forward) != 0 {
		t.Fatalf("文本帧被转发到了节点侧: %q", forward)
	}
}
