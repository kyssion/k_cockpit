package vm

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm/clause"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// InterfaceView 是网卡在接口层的形态。
type InterfaceView struct {
	ID int64 `json:"id"`
	// Order 是网卡序号，同时也是它在**来宾系统里的设备顺序**。
	//
	// 界面应以它为主标识而不是数据库 id：重建网卡会得到新 id，而 eth0/eth1
	// 是按顺序认的。
	Order     int   `json:"order"`
	IsPrimary bool  `json:"is_primary"`
	NodeID    int64 `json:"node_id"`

	Model string `json:"model"`

	// SwitchID 为空表示使用节点默认网络。
	SwitchID   *int64  `json:"switch_id,omitempty"`
	SwitchName *string `json:"switch_name,omitempty"`

	MAC              *string `json:"mac,omitempty"`
	AllowedAddresses *string `json:"allowed_addresses,omitempty"`
	RateLimitMbps    int     `json:"rate_limit_mbps"`

	// Applied 报告配置是否已下发到节点。
	//
	// 用布尔而不是让前端自己判断时间戳：前端判断等于把「多久算没生效」
	// 这条规则复制一份，两处迟早不一致。
	Applied       bool       `json:"applied"`
	LastAppliedAt *time.Time `json:"last_applied_at,omitempty"`
}

// StaticIPView 是静态地址在接口层的形态。
type StaticIPView struct {
	ID            int64  `json:"id"`
	IP            string `json:"ip"`
	AddressFamily string `json:"address_family"`

	InterfaceOrder *int    `json:"interface_order,omitempty"`
	MAC            *string `json:"mac,omitempty"`

	// IsDHCPReservation 区分静态租约与来宾内手工配置的地址。
	// 两者的排查方向完全不同：前者查 DHCP 服务，后者要进系统看配置。
	IsDHCPReservation bool `json:"is_dhcp_reservation"`

	Applied   bool       `json:"applied"`
	AppliedAt *time.Time `json:"applied_at,omitempty"`
}

// Interfaces 返回虚拟机的网卡列表。
//
// 归属校验复用 load：不属于当前视角的返回 **404 而非 403**，否则可以据此
// 枚举他人有哪些虚拟机。
func (s *Service) Interfaces(ctx context.Context, id int64, v authz.Viewer) ([]InterfaceView, error) {
	if _, err := s.load(ctx, id, v); err != nil {
		return nil, err
	}

	var rows []model.VMInterface
	err := s.db.WithContext(ctx).
		Where("vm_id = ?", id).
		// `order` 是 SQL 保留字，必须走 clause 让 GORM 加引号；
		// 直接写 Order("order") 在 PostgreSQL 上是语法错误。
		Order(clause.OrderByColumn{Column: clause.Column{Name: "order"}}).
		Find(&rows).Error
	if err != nil {
		log.Printf("[vm] 查询网卡失败 vm=%d: %v", id, err)
		return nil, api.Internal()
	}

	names, err := s.switchNames(ctx, rows)
	if err != nil {
		return nil, err
	}

	out := make([]InterfaceView, 0, len(rows))
	for _, r := range rows {
		view := InterfaceView{
			ID: r.ID, Order: r.Order, IsPrimary: r.IsPrimary, NodeID: r.NodeID,
			Model:            r.Model,
			SwitchID:         r.SwitchID,
			MAC:              r.MAC,
			AllowedAddresses: r.AllowedAddresses,
			RateLimitMbps:    r.RateLimitMbps,
			Applied:          r.LastAppliedAt != nil,
			LastAppliedAt:    r.LastAppliedAt,
		}
		if r.SwitchID != nil {
			if name, ok := names[*r.SwitchID]; ok {
				view.SwitchName = &name
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// switchNames 批量取交换机名字。
//
// 逐条查会产生 N+1 次往返——一台虚拟机几个网卡还看不出来，列表页放大就明显了。
func (s *Service) switchNames(
	ctx context.Context, rows []model.VMInterface,
) (map[int64]string, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.SwitchID != nil {
			ids = append(ids, *r.SwitchID)
		}
	}

	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	var switches []model.VpcSwitch
	err := s.db.WithContext(ctx).
		Select("id", "name").
		Where("id IN ?", ids).
		Find(&switches).Error
	if err != nil {
		log.Printf("[vm] 查询交换机失败: %v", err)
		return nil, api.Internal()
	}
	for _, sw := range switches {
		out[sw.ID] = sw.Name
	}
	return out, nil
}

// StaticIPs 返回虚拟机的静态地址列表。
//
// 只返回**已绑定到这台虚拟机**的记录：vm_id 为空的地址是预留但未分配的，
// 把它显示在这里会让用户以为虚拟机已经拿到了那个 IP。
func (s *Service) StaticIPs(ctx context.Context, id int64, v authz.Viewer) ([]StaticIPView, error) {
	if _, err := s.load(ctx, id, v); err != nil {
		return nil, err
	}

	var rows []model.StaticIP
	err := s.db.WithContext(ctx).
		Where("vm_id = ?", id).
		Order("ip").
		Find(&rows).Error
	if err != nil {
		log.Printf("[vm] 查询静态地址失败 vm=%d: %v", id, err)
		return nil, api.Internal()
	}

	out := make([]StaticIPView, 0, len(rows))
	for _, r := range rows {
		out = append(out, StaticIPView{
			ID: r.ID, IP: r.IP, AddressFamily: r.AddressFamily,
			InterfaceOrder:    r.InterfaceOrder,
			MAC:               r.MAC,
			IsDHCPReservation: r.IsDHCPReservation,
			Applied:           r.AppliedAt != nil,
			AppliedAt:         r.AppliedAt,
		})
	}
	return out, nil
}
