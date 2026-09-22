-- 凭据表唯一索引修正（G-30）。
--
-- 原索引 uniq_vm_credential_vm_id 只约束 vm_id，意味着一台虚拟机只能有一行
-- 凭据。但表设计是「用 username 区分用途」：控制台密码（_vnc）与创建时注入
-- 的初始登录密码（root / Administrator）是两行。两者并存时后写的一方会撞
-- 唯一索引而静默失败——表现为「设了控制台密码，初始凭据不见了」或反之。
-- 改为 (vm_id, username) 复合唯一：一台虚拟机每个用途各一行。
DROP INDEX IF EXISTS uniq_vm_credential_vm_id;
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_credential_vm_username ON vm_credential (vm_id, username);
