// enroll.go：一次性注册的 agent 侧实现。
package nodeagent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
)

// EnrollInput 是一次注册请求。
type EnrollInput struct {
	Server  string
	Token   string
	AgentID string
	Version string
	// TLS 是 enroll 连接的 TLS 配置（StateDir.EnrollTLS：无客户端证书）。
	TLS *tls.Config
}

// EnrollResult 是注册成功后的全部材料，调用方负责持久化（StateDir.SaveEnrollment）。
type EnrollResult struct {
	State    State
	CaPEM    []byte
	CertPEM  []byte
	KeyPEM   []byte
	NodeName string
}

// Enroll 用一次性令牌换取客户端证书。
//
// 注册语义（令牌校验、状态翻转、审计）在**控制面**的 node.Service 里；
// agent 侧只负责把请求送上去、把材料保存下来。
func Enroll(ctx context.Context, in EnrollInput) (*EnrollResult, error) {
	if in.Server == "" || in.Token == "" {
		return nil, fmt.Errorf("server 与 token 必填")
	}
	if in.TLS == nil {
		return nil, fmt.Errorf("未配置 enroll TLS")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, in.Server,
		grpc.WithTransportCredentials(credentials.NewTLS(in.TLS)),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("连接控制面: %w", err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := agentproto.NewAgentGatewayClient(conn).Register(dialCtx, &agentproto.RegisterRequest{
		Token:           in.Token,
		AgentId:         in.AgentID,
		AgentVersion:    in.Version,
		ProtocolVersion: ProtocolVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("注册被拒绝: %w", err)
	}
	if resp.AgentId == "" {
		return nil, fmt.Errorf("控制面未返回 agent 身份")
	}
	return &EnrollResult{
		State: State{
			AgentID: resp.AgentId,
			Server:  in.Server,
		},
		CaPEM:    resp.CaCert,
		CertPEM:  resp.ClientCert,
		KeyPEM:   resp.ClientKey,
		NodeName: resp.NodeName,
	}, nil
}

// NewRegistry 返回本构建的执行器注册表。
//
// 注册即能力（Capabilities 由注册表生成）。P1 覆盖全部只读探测，P2 覆盖
// vm 生命周期与控制台；网络/存储/模板类操作随 P3~P4 逐个注册——未注册的
// 操作返回明确的「未实现」结果，能力清单永远与实际可执行的操作一致。
func NewRegistry() Registry {
	return Registry{
		// node 域
		agent.OpNodeStats:   nodeStats,
		agent.OpNodeDisks:   nodeDisks,
		agent.OpNodeNetwork: nodeNetwork,
		agent.OpNodeDomains: nodeDomains,
		// host 域
		agent.OpHostStats:       hostStats,
		agent.OpHostHardware:    hostHardware,
		agent.OpHostNetStats:    hostNetstats,
		agent.OpHostTuning:      hostTuning,
		agent.OpHostIOMMU:       hostIOMMU,
		agent.OpHostPCIDevices:  hostPCIDevices,
		agent.OpHostConnections: hostConnections,
		// vm 域（只读）
		agent.OpVMStatus:    vmStatus,
		agent.OpVMStats:     vmStats,
		agent.OpVMDiskList:  vmDiskList,
		agent.OpVMXML:       vmXML,
		agent.OpVMPcieInfo:  vmPCIeInfo,
		agent.OpVMNeighbors: vmNeighbor,
		// 网络探测
		agent.OpNetworkProbe:  networkProbe,
		agent.OpOVSStatus:     ovsStatus,
		agent.OpOVSPorts:      ovsPorts,
		agent.OpDHCPLeases:    dhcpLeases,
		agent.OpPlatformCheck: platformCheck,
		agent.OpMigrateAssess: migrateAssess,
		// vm 写路径与控制台（P2）
		agent.OpVMCreate:          vmCreate,
		agent.OpVMClone:           vmCreate,
		agent.OpVMStart:           powerOp("start", "start"),
		agent.OpVMShutdown:        powerOp("shutdown", "shutdown"),
		agent.OpVMPoweroff:        powerOp("poweroff", "destroy"),
		agent.OpVMReboot:          powerOp("reboot", "reboot"),
		agent.OpVMReset:           powerOp("reset", "reset"),
		agent.OpVMDelete:          vmDelete,
		agent.OpVMSnapshotCreate:  vmSnapshotCreate,
		agent.OpVMSnapshotRestore: vmSnapshotRestore,
		agent.OpVMSnapshotDelete:  vmSnapshotDelete,
		agent.OpVMConfigUpdate:    vmConfigUpdate,
		agent.OpVMCDROMApply:      vmCDROMApply,
		agent.OpVMFloppyApply:     vmFloppyApply,
		agent.OpVMNVRAMRepair:     vmNVRAMRepair,
		agent.OpVMConsoleFrame:    vmConsoleFrame,
		// 网络写路径（P3 第一批）
		agent.OpVPCSwitchChange:      vpcSwitchChange,
		agent.OpNetworkBridgeApply:   networkBridgeApply,
		agent.OpNetworkBridgeDelete:  networkBridgeDelete,
		agent.OpNetworkUplinkAttach:  networkUplinkAttach,
		agent.OpNetworkUplinkConfirm: networkUplinkConfirm,
		agent.OpNetworkUplinkDetach:  networkUplinkDetach,
		agent.OpNetworkRepair:        networkRepair,
		// 网络安全（P3 第二批）
		agent.OpSecurityGroupApply:  securityGroupApply,
		agent.OpFirewallApply:       firewallApply,
		agent.OpFirewallRollback:    firewallRollback,
		agent.OpHostFirewallApply:   hostFirewallApply,
		agent.OpVMPortForwardChange: vmPortForwardChange,
		agent.OpVMStaticIPChange:    vmStaticIPChange,
		agent.OpPublicIPChange:      publicIPChange,
		agent.OpPortSecurityApply:   portSecurityApply,
		agent.OpPortMirrorEnable:    portMirrorEnable,
		agent.OpPortMirrorConfirm:   portMirrorConfirm,
		agent.OpPortMirrorDisable:   portMirrorDisable,
		agent.OpNetworkCapture:      networkCapture,
		agent.OpVMInitReady:         vmInitReady,
		// 存储与模板（P4）
		agent.OpStoragePoolCreate:      storagePoolCreate,
		agent.OpStoragePoolDelete:      storagePoolDelete,
		agent.OpStoragePoolScan:        storagePoolScan,
		agent.OpStoragePoolConfig:      storagePoolConfig,
		agent.OpStoragePoolUnmount:     storagePoolUnmount,
		agent.OpStorageTrim:            storageTrim,
		agent.OpStoragePartitionCreate: storagePartitionCreate,
		agent.OpStoragePartitionDelete: storagePartitionDelete,
		agent.OpStorageVolumeApply:     storageVolumeApply,
		agent.OpStorageFileCommit:      storageFileCommit,
		agent.OpStorageFileRead:        storageFileRead,
		agent.OpShareMount:             vmShareMount,
		// P5 长尾
		agent.OpVMMigrate:             vmMigrate,
		agent.OpVMMigrateTakeover:     vmMigrateTakeover,
		agent.OpVMReinstall:           vmReinstall,
		agent.OpVMReinstallPurge:      vmReinstallPurge,
		agent.OpVMRescueEnter:         vmRescueEnter,
		agent.OpVMRescueExit:          vmRescueExit,
		agent.OpVMExport:              vmExport,
		agent.OpVMExportDelete:        vmExportDelete,
		agent.OpVMExportFetch:         vmExportFetch,
		agent.OpVMGuest:               vmGuest,
		agent.OpSecurityPasswordAudit: securityPasswordAudit,
		agent.OpUserSSHAccess:         userSSHAccess,
		agent.OpQuotaEnforce:          quotaEnforce,
		agent.OpPlatformRepair:        platformRepair,
	}
}

// jsonMarshal / jsonUnmarshalTo 是 ex_net.go 里期望解码用的小助手。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func jsonUnmarshalTo(b []byte, v any) error { return json.Unmarshal(b, v) }
