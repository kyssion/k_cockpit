-- SPICE 控制台（F-2-09）。
--
-- 做成**控制台的协议维度**，而不是另起一套接口。理由是整个功能里最要紧的
-- 一条：SPICE 的「对外暴露」与 VNC 是**同一类风险**——都是暴露一个远程控制
-- 入口。另起一套接口意味着把二次验证、监听地址切换、警告文案再写一遍，而
-- 那两份迟早会分叉：某天有人给 VNC 那条加了更严的限制，SPICE 那条还开着。
--
-- 因此字段与 VNC 平行，而**判定与文案共用同一段代码**。

-- 由节点探测填入（见 model 上的说明）。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS spice_supported boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS spice_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS spice_port integer;

-- 与 vnc_bind 同理，默认只听本地（R-001）。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS spice_bind varchar(64) NOT NULL DEFAULT '127.0.0.1';

-- 与 vnc_exposed 分开记录，而不是从监听地址推断：让「曾经暴露过」这件事
-- 留下痕迹。从 bind 推断的话，一次「暴露后又收回」会看起来像从未暴露过。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS spice_exposed boolean NOT NULL DEFAULT false;
