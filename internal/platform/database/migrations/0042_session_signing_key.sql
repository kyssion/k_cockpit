-- 会话令牌的签名密钥（F-1-09：密钥轮换）。
--
-- 密钥单独存表而不是只放在配置里：放在配置里也能换，但换一次就要改一次
-- 环境变量并重启，而"轮换"这件事的价值恰恰在于**能随时做**——怀疑泄漏
-- 的时候不会有窗口去安排一次重启。
--
-- 存的是**加密后的密钥**（用配置里的根密钥派生），而不是明文：数据库备份
-- 泄漏时不至于直接拿到可以伪造任意会话的密钥。
--
-- 只保留"当前在用"这一条：轮换的语义是**让全部旧令牌立即失效**（注释见
-- auth/token.go 的 R-012），因此不需要为旧密钥留宽限期——留了就等于轮换
-- 之后旧令牌还能用一段时间，而那正是轮换要消除的东西。
CREATE TABLE IF NOT EXISTS session_signing_key (
    id          bigserial    PRIMARY KEY,
    key_id      varchar(32)  NOT NULL,
    secret_enc  text         NOT NULL,
    active      boolean      NOT NULL DEFAULT true,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    rotated_at  timestamptz  NOT NULL DEFAULT now(),
    rotated_by  bigint
);

CREATE INDEX IF NOT EXISTS idx_session_signing_key_active
    ON session_signing_key (active);
