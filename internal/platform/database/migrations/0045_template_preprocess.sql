-- 模板离线预处理（F-3-06）。
--
-- preprocessed_at 记录上次预处理完成的时刻。它是**状态**而不是开关：预处理做过之后镜像就变了，再处理一次要重新走一遍流程。
--
-- 为什么没有"预处理选项"列：选项是每次执行时选的，存一列等于把一次性的执行参数当成模板的固有属性，而下一次很可能选不同的项（这次装 agent，下次只清 machine-id）。
ALTER TABLE template ADD COLUMN IF NOT EXISTS preprocessed_at timestamptz;
