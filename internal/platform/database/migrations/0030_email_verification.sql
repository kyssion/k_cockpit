-- 邮箱验证码（F-1-08：邮箱绑定与找回密码）。
--
-- **存哈希而不是明文**：验证码是「知道即可登录/改密」的凭据，与密码同级。
-- 明文入库意味着一次只读的 SQL 注入或备份泄漏就能直接接管账号，而哈希
-- 让库里的这份数据只对「校验用户输入的那个字符串」有用。
--
-- **保留已消费的记录**：consumed_at 记下使用时刻，用来回答"这个码是不是
-- 刚被用过"。删掉它的话，重复提交会表现为「验证码错误」，而用户坚称自己
-- 只点了一次——那种争议无法从数据上裁决。
--
-- 过期时间写进表里而不是常量：同一段代码要给「绑定邮箱」和「找回密码」
-- 两种场景发码，它们的有效期本就应该不同（找回密码更短）。

CREATE TABLE IF NOT EXISTS email_verification (
    id          bigserial    PRIMARY KEY,
    user_id     bigint,
    email       varchar(128) NOT NULL,
    kind        varchar(16)  NOT NULL,
    code_hash   varchar(255) NOT NULL,
    attempts    int          NOT NULL DEFAULT 0,
    expires_at  timestamptz  NOT NULL,
    consumed_at timestamptz,
    -- ticket_used_at 记下重置票据被使用的时刻。
    --
    -- 票据本身是无状态的 JWT，签名有效就能重复提交；没有这一列，"改密码"
    -- 这个动作在票据有效期内可以被重放任意次——用户改完密码后，拿着同一
    -- 个链接的人还能再改一次。
    ticket_used_at timestamptz,
    client_ip   varchar(64),
    created_at  timestamptz  NOT NULL DEFAULT now()
);

-- 按 (email, kind) 取最近一条：校验时只认最新发出的那个码，旧的自然作废。
CREATE INDEX IF NOT EXISTS idx_email_verification_lookup
    ON email_verification (email, kind, created_at DESC);
