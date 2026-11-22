-- +goose Up
-- Per-user daily AI usage (backend TRD 10.2). day is the UTC date, like ai:cost.
-- user_id has no foreign key: this is a stats table and must not block account deletion (B7).
create table ai_usage_daily (
    day        date   not null,
    user_id    uuid   not null,
    kind       text   not null check (kind in ('translate','polish')),
    calls      int    not null,
    tokens_in  bigint not null,
    tokens_out bigint not null,
    cost_micro bigint not null,
    primary key (day, user_id, kind)
);

-- +goose Down
drop table if exists ai_usage_daily;
