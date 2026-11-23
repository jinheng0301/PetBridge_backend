-- +goose Up
-- B6a: content moderation, blocks, reports (spec 5.3, 6.1, 7.1).
alter table deliveries
    add column mod_status text not null default 'ok' check (mod_status in ('ok','review','block'));
alter table messages
    add constraint messages_mod_status_check check (mod_status in ('ok','review','block'));

-- Only user_id has a foreign key: messages and deliveries may be purged while the flag stays.
create table mod_flags (
    id          uuid primary key default gen_random_uuid(),
    created_at  timestamptz not null default now(),
    stage       text not null check (stage in ('outgoing','translation','polish','nickname')),
    verdict     text not null check (verdict in ('review','block')),
    source      text not null check (source in ('wordlist','model','both')),
    terms       text[] not null default '{}',
    user_id     uuid not null references users(id),
    room_id     uuid,
    message_id  uuid,
    delivery_id uuid,
    target_lang text,
    text        text,
    status      text not null default 'open' check (status in ('open','reviewed'))
);
create index idx_mod_flags_status_created on mod_flags(status, created_at);

create table blocks (
    blocker_id uuid not null references users(id),
    blocked_id uuid not null references users(id),
    created_at timestamptz not null default now(),
    primary key (blocker_id, blocked_id),
    check (blocker_id <> blocked_id)
);
create index idx_blocks_blocked on blocks(blocked_id);

create table reports (
    id           uuid primary key default gen_random_uuid(),
    reporter_id  uuid not null references users(id),
    target_id    uuid not null references users(id),
    room_id      uuid not null,
    message_id   uuid,
    reason       text not null check (reason in ('harassment','spam','sexual','self_harm','minor_safety','other')),
    note         text,
    status       text not null default 'open' check (status in ('open','resolved','dismissed')),
    created_at   timestamptz not null default now(),
    handled_at   timestamptz,
    handled_note text
);
create index idx_reports_status_created on reports(status, created_at);
create index idx_reports_reporter_created on reports(reporter_id, created_at);
create unique index uq_reports_open on reports(reporter_id, target_id, coalesce(message_id, '00000000-0000-0000-0000-000000000000'::uuid))
    where status = 'open';

-- +goose Down
drop table if exists reports;
drop table if exists blocks;
drop table if exists mod_flags;
alter table messages drop constraint if exists messages_mod_status_check;
alter table deliveries drop column if exists mod_status;
