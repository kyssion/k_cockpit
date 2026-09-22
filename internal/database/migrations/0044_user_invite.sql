-- 邀请注册（F-1-10）。
--
-- 为什么要有邀请而不是"开放注册"：这个面板能接管宿主机上的全部虚拟机，
-- 开放注册等于把入口交给任何人。邀请把"谁能进来"变成一次**由管理员发出的、
-- 有期限的、可追溯**的动作。
--
-- 存的是令牌的**哈希**而不是令牌本身：链接会出现在邮件、聊天记录与浏览器
-- 历史里，而库里的哈希即使泄漏也换不回那个链接。
CREATE TABLE IF NOT EXISTS user_invite (
    id            bigserial    PRIMARY KEY,
    email         varchar(128) NOT NULL,
    role          varchar(16)  NOT NULL DEFAULT 'tenant',
    token_hash    varchar(128) NOT NULL,
    -- 配额：邀请时就把额度定下来，避免"先进来再慢慢谈额度"这种顺序——
    -- 那时他已经能建机器了。
    quota_bytes   bigint       NOT NULL DEFAULT 0,
    quota_enabled boolean      NOT NULL DEFAULT false,
    remark        varchar(255),

    expires_at    timestamptz  NOT NULL,
    accepted_at   timestamptz,
    -- accepted_user_id 让"这个账号是从哪条邀请来的"可追溯。
    accepted_user_id bigint,
    revoked_at    timestamptz,

    created_by    bigint,
    created_at    timestamptz  NOT NULL DEFAULT now()
);

-- 按邮箱查未使用的邀请：重发时要先找到它，而不是又造一条。
CREATE INDEX IF NOT EXISTS idx_user_invite_email ON user_invite (email);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_invite_token ON user_invite (token_hash);
