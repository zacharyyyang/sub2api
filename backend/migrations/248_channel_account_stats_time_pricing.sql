-- Account stats time-of-day pricing: per-entry IANA timezone plus a period table
-- carrying an independent full price set for each time-of-day window.
-- Column shapes, defaults and the index mirror migration 106 (account stats pricing intervals).
ALTER TABLE channel_account_stats_model_pricing
    ADD COLUMN IF NOT EXISTS stats_time_pricing_timezone TEXT NULL;

COMMENT ON COLUMN channel_account_stats_model_pricing.stats_time_pricing_timezone IS
    'IANA timezone for account stats time-of-day pricing; NULL when the entry has no time pricing';

CREATE TABLE IF NOT EXISTS channel_account_stats_pricing_periods (
    id                   BIGSERIAL      PRIMARY KEY,
    pricing_id           BIGINT         NOT NULL REFERENCES channel_account_stats_model_pricing(id) ON DELETE CASCADE,
    start_time           VARCHAR(8)     NOT NULL,
    end_time             VARCHAR(8)     NOT NULL,
    input_price          NUMERIC(20,12),
    output_price         NUMERIC(20,12),
    cache_write_price    NUMERIC(20,12),
    cache_write_1h_price NUMERIC(20,12),
    cache_read_price     NUMERIC(20,12),
    per_request_price    NUMERIC(20,12),
    sort_order           INT            NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ    NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_account_stats_pricing_periods_pricing_id
    ON channel_account_stats_pricing_periods (pricing_id);