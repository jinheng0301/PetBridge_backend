-- +goose Up
-- B6b: daily check-ins, room energy and decorations (spec chapter 6).
create table checkins (
    room_id    uuid not null references rooms(id),
    user_id    uuid not null references users(id),
    local_day  date not null,
    energy     int not null check (energy > 0),
    bonus_at   timestamptz, -- when a full-house bonus used this check-in; null = unused
    created_at timestamptz not null,
    primary key (room_id, user_id, local_day)
);
create index idx_checkins_room_created on checkins(room_id, created_at);

-- No row means energy 0.
create table room_progress (
    room_id      uuid primary key references rooms(id),
    energy_total int not null default 0 check (energy_total >= 0),
    updated_at   timestamptz not null
);

create table room_decor (
    room_id     uuid not null references rooms(id),
    item_id     text not null,
    unlocked_at timestamptz not null,
    primary key (room_id, item_id)
);

-- +goose Down
drop table if exists room_decor;
drop table if exists room_progress;
drop table if exists checkins;
