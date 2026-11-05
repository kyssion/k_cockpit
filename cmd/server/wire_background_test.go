package main

import (
	"context"
	"testing"
	"time"
)

// stopLoops 必须等所有以 ctx 为生命周期的循环真的退出，否则进程退出时
// 会留下还在读写库的 goroutine。
func TestStopLoopsWaitsForCancelledLoops(t *testing.T) {
	var loops backgroundLoops
	ctx, cancel := context.WithCancel(context.Background())
	loops.cancel = cancel

	exited := make(chan struct{})
	loops.goLoop(func() {
		<-ctx.Done()
		close(exited)
	})

	done := make(chan struct{})
	go func() {
		loops.stopLoops()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopLoops 未返回：循环没有被取消或 WaitGroup 漏了 Done")
	}
	select {
	case <-exited:
	default:
		t.Fatal("stopLoops 返回时循环仍未退出")
	}
}
