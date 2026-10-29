-- 计价币种：定价条目可声明自身价格是美元还是人民币。
--
-- 背景：内置价表（LiteLLM / fallback）是美元标度，国产渠道（如百度/千帆）录入的是
-- 人民币价；两者此前共用同一符号与同一数值口径。本迁移只加"声明币种"的位置，
-- 不做数值改写、不猜历史意图——存量行一律 USD，需要 CNY 的由管理员逐条改。
ALTER TABLE channel_model_pricing
    ADD COLUMN IF NOT EXISTS price_currency VARCHAR(3) NOT NULL DEFAULT 'USD';

COMMENT ON COLUMN channel_model_pricing.price_currency IS
    'Entry price currency: USD prices are multiplied by the USD→CNY rate before use; CNY prices are used as-is';

ALTER TABLE channel_account_stats_model_pricing
    ADD COLUMN IF NOT EXISTS price_currency VARCHAR(3) NOT NULL DEFAULT 'USD';

COMMENT ON COLUMN channel_account_stats_model_pricing.price_currency IS
    'Stats cost entry price currency: USD is converted by the USD→CNY rate; CNY is used as-is';

ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS default_price_currency VARCHAR(3) NOT NULL DEFAULT 'USD';

COMMENT ON COLUMN channels.default_price_currency IS
    'Default currency pre-filled for new pricing entries of this channel (USD or CNY)';