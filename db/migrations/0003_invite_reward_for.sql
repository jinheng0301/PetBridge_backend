-- +goose Up
alter table invite_codes add column reward_for text references invite_codes(code);
create index idx_invite_codes_reward_for on invite_codes(reward_for);

-- +goose Down
drop index if exists idx_invite_codes_reward_for;
alter table invite_codes drop column if exists reward_for;
