-- +goose Up
create table users (
    id              uuid primary key default gen_random_uuid(),
    nickname        text,
    native_lang     text check (native_lang in ('ja','ko','zh-Hant','zh-Hans','en')),
    mbti            text check (mbti ~ '^[EI][NS][TF][JP]$'),
    pet_type        text,
    age_band        text check (age_band in ('13_17','18_plus')),
    tz              text,
    region          text,
    mbti_changed_at timestamptz,
    onboarded_at    timestamptz,
    status          text not null default 'active' check (status in ('active','banned','deleted')),
    deleted_at      timestamptz,
    created_at      timestamptz not null default now()
);

create table auth_identities (
    id         bigserial primary key,
    provider   text not null check (provider in ('apple','google','dev')),
    subject    text not null,
    user_id    uuid not null references users(id),
    created_at timestamptz not null default now(),
    unique (provider, subject)
);
create index idx_auth_identities_user on auth_identities(user_id);

create table refresh_tokens (
    id         uuid primary key default gen_random_uuid(),
    user_id    uuid not null references users(id),
    token_hash text not null unique,
    device_id  text,
    expires_at timestamptz not null,
    revoked_at timestamptz,
    created_at timestamptz not null default now()
);
create index idx_refresh_tokens_user on refresh_tokens(user_id);

create table devices (
    id               uuid primary key default gen_random_uuid(),
    user_id          uuid not null references users(id),
    platform         text not null check (platform in ('ios','android')),
    push_token       text,
    locale           text,
    widget_installed boolean not null default false,
    last_seen_at     timestamptz
);
create index idx_devices_user on devices(user_id);

create table rooms (
    id           uuid primary key default gen_random_uuid(),
    owner_id     uuid not null unique references users(id),
    capacity     int not null default 5 check (capacity > 0),
    member_count int not null default 0,
    status       text not null default 'open' check (status in ('open','full','closed')),
    created_at   timestamptz not null default now(),
    constraint ck_member_cap check (member_count <= capacity)
);

create table room_members (
    id          bigserial primary key,
    room_id     uuid not null references rooms(id),
    user_id     uuid not null references users(id),
    seat_no     smallint not null check (seat_no between 1 and 5),
    role        text not null check (role in ('owner','member')),
    joined_at   timestamptz not null default now(),
    left_at     timestamptz,
    left_reason text check (left_reason in ('left','kicked','dissolved')),
    check ((left_at is null) = (left_reason is null))
);
create unique index uq_member_active on room_members(room_id, user_id) where left_at is null;
create unique index uq_seat_active   on room_members(room_id, seat_no) where left_at is null;
create index idx_room_members_user on room_members(user_id);

create table room_bans (
    room_id    uuid not null references rooms(id),
    user_id    uuid not null references users(id),
    banned_by  uuid references users(id),
    created_at timestamptz not null default now(),
    primary key (room_id, user_id)
);

create table invite_codes (
    code       text constraint pk_code primary key check (code ~ '^[2-9A-HJ-NP-Z]{8}$'),
    room_id    uuid not null references rooms(id),
    issued_to  uuid references users(id),
    source     text not null check (source in ('zero','signup','join_reward','owner_regen')),
    status     text not null default 'unused' check (status in ('unused','used','expired','void')),
    used_by    uuid references users(id),
    used_at    timestamptz,
    expires_at timestamptz not null,
    issued_at  timestamptz not null default now()
);
create index idx_invite_codes_room_status on invite_codes(room_id, status);
create index idx_invite_codes_issued_to   on invite_codes(issued_to);

create table reward_grants (
    user_id       uuid not null references users(id),
    room_id       uuid not null references rooms(id),
    granted_count int not null,
    created_at    timestamptz not null default now(),
    primary key (user_id, room_id)
);

create table invite_edges (
    id         bigserial primary key,
    inviter_id uuid not null references users(id),
    invitee_id uuid not null references users(id),
    code       text not null unique references invite_codes(code),
    created_at timestamptz not null default now()
);

create table app_config (
    key        text primary key,
    value      jsonb not null,
    updated_at timestamptz not null default now()
);

-- +goose Down
drop table if exists app_config;
drop table if exists invite_edges;
drop table if exists reward_grants;
drop table if exists invite_codes;
drop table if exists room_bans;
drop table if exists room_members;
drop table if exists rooms;
drop table if exists devices;
drop table if exists refresh_tokens;
drop table if exists auth_identities;
drop table if exists users;
