// 模拟节点预置（开发期专用，ADR-0007 mock-first）。
//
// 面板的功能演示（创建向导的完整步骤、迁移目标清单、配额聚合）都以
// 「已有在线节点」为前提；mock 运输层下每次起服务都手工接入三台太啰嗦，
// 因此由装配层在开发环境调用 EnsureSimulated 预置。生产环境不进入
// 这条路径（main 里有环境与运输层双重闸门）。
package node

import (
	"context"
	"fmt"
	"log"

	"k_cockpit/internal/model"
)

// EnsureSimulated 预置名字不在库中的模拟节点，已存在的原样跳过——
// 启动期每次都会调用，必须幂等。
//
// 刻意走真实的 CreateEnrollToken → Register 链路而不是直接插行：与
// 真实 agent 共用同一段注册逻辑（ADR-0007 的原则），预置出来的节点
// 与手工接入的没有任何差别。
func (s *Service) EnsureSimulated(ctx context.Context, names []string) error {
	var rows []model.Node
	if err := s.db.WithContext(ctx).Select("name").Find(&rows).Error; err != nil {
		return fmt.Errorf("查询现有节点: %w", err)
	}
	existing := make(map[string]struct{}, len(rows))
	for _, n := range rows {
		existing[n.Name] = struct{}{}
	}

	for _, name := range names {
		if _, ok := existing[name]; ok {
			continue
		}
		tok, err := s.CreateEnrollToken(ctx, name, DefaultEnrollTTL, 0, "system-simulate", "127.0.0.1")
		if err != nil {
			return fmt.Errorf("为模拟节点 %s 签发注册令牌: %w", name, err)
		}
		// agentID 带上 mock 前缀：列表页据此识别并提示「模拟数据」。
		if _, err := s.Register(ctx, tok.Token, "mock-agent-"+name, "mock-0.1.0"); err != nil {
			return fmt.Errorf("注册模拟节点 %s: %w", name, err)
		}
		log.Printf("[node] 已预置模拟节点 %s", name)
	}
	return nil
}
