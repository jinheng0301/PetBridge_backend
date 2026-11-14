-- +goose Up
alter table refresh_tokens add column family_id uuid not null default gen_random_uuid();
alter table refresh_tokens alter column family_id drop default;
alter table refresh_tokens add column replaced_by uuid;
create index idx_refresh_tokens_family on refresh_tokens(family_id);

-- +goose Down
drop index if exists idx_refresh_tokens_family;
alter table refresh_tokens drop column if exists replaced_by;
alter table refresh_tokens drop column if exists family_id;
