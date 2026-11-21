-- +goose Up
create table messages (
    id               uuid primary key,
    room_id          uuid not null references rooms(id),
    sender_id        uuid not null references users(id),
    scope            text not null check (scope in ('room','dm')),
    to_user_id       uuid references users(id),
    kind             text not null check (kind in ('text','phrase')),
    phrase_id        text,
    src_lang         text not null,
    body             text not null,
    client_msg_id    text not null,
    polish_requested boolean not null default false,
    mod_status       text not null default 'ok',
    created_at       timestamptz not null default now(),
    unique (sender_id, client_msg_id),
    check ((scope = 'dm') = (to_user_id is not null))
);
create index idx_messages_room_id on messages(room_id, id);

create table deliveries (
    id                 uuid primary key default gen_random_uuid(),
    message_id         uuid not null references messages(id) on delete cascade,
    receiver_id        uuid not null references users(id),
    target_lang        text not null,
    base_text          text,
    translation_status text not null check (translation_status in ('pending','done','failed')),
    translate_attempts int not null default 0,
    polished_text      text,
    polish_status      text not null default 'none' check (polish_status in ('none','pending','done','fallback')),
    polish_by          uuid references users(id),
    read_at            timestamptz,
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    unique (message_id, receiver_id)
);
create index idx_deliveries_receiver_message on deliveries(receiver_id, message_id);
create index idx_deliveries_pending_scan on deliveries(updated_at)
    where translation_status in ('pending','failed') or polish_status = 'pending';

-- +goose Down
drop table if exists deliveries;
drop table if exists messages;
