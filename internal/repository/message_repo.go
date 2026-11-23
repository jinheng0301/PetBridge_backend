package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

// MessageRepo stores messages and their per-receiver deliveries.
type MessageRepo struct{ pool *pgxpool.Pool }

func NewMessageRepo(pool *pgxpool.Pool) *MessageRepo { return &MessageRepo{pool: pool} }

const messageCols = `m.id::text, m.room_id::text, m.sender_id::text, m.scope, m.to_user_id::text, m.kind, m.phrase_id,
	m.src_lang, m.body, m.client_msg_id, m.polish_requested, m.created_at, u.nickname, u.pet_type`

// scanMessage reads the columns of messageCols; the caller chooses which view to expose.
func scanMessage(row pgx.Row) (model.MessageView, string, error) {
	var v model.MessageView
	var cmid string
	err := row.Scan(&v.ID, &v.RoomID, &v.Sender.UserID, &v.Scope, &v.ToUserID, &v.Kind, &v.PhraseID,
		&v.SrcLang, &v.Text, &cmid, &v.PolishRequested, &v.CreatedAt, &v.Sender.Nickname, &v.Sender.PetType)
	return v, cmid, err
}

// ActiveRecipients returns the seated members of the room with native language and age band ("" when unset).
func (r *MessageRepo) ActiveRecipients(ctx context.Context, roomID string) ([]model.Recipient, error) {
	if !uuidRE.MatchString(roomID) {
		return nil, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select u.id::text, coalesce(u.native_lang,''), coalesce(u.age_band,'')
		from room_members rm join users u on u.id = rm.user_id
		where rm.room_id = $1 and rm.left_at is null
		order by rm.seat_no`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Recipient
	for rows.Next() {
		var x model.Recipient
		if err := rows.Scan(&x.UserID, &x.NativeLang, &x.AgeBand); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// InsertMessage writes the row unless (sender_id, client_msg_id) already exists.
func (r *MessageRepo) InsertMessage(ctx context.Context, m model.NewMessage) (bool, error) {
	modStatus := m.ModStatus
	if modStatus == "" {
		modStatus = model.ModOK
	}
	tag, err := conn(ctx, r.pool).Exec(ctx, `
		insert into messages(id, room_id, sender_id, scope, to_user_id, kind, phrase_id, src_lang, body, client_msg_id, polish_requested, mod_status)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		on conflict (sender_id, client_msg_id) do nothing`,
		m.ID, m.RoomID, m.SenderID, m.Scope, m.ToUserID, m.Kind, m.PhraseID, m.SrcLang, m.Body, m.ClientMsgID, m.PolishRequested, modStatus)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *MessageRepo) FindByClientMsgID(ctx context.Context, senderID, clientMsgID string) (string, error) {
	if !uuidRE.MatchString(senderID) {
		return "", model.ErrNotFound
	}
	var id string
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select id::text from messages where sender_id=$1 and client_msg_id=$2`, senderID, clientMsgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrNotFound
	}
	return id, err
}

func (r *MessageRepo) InsertDeliveries(ctx context.Context, messageID string, ds []model.NewDelivery) error {
	db := conn(ctx, r.pool)
	for _, d := range ds {
		if _, err := db.Exec(ctx, `
			insert into deliveries(message_id, receiver_id, target_lang, base_text, translation_status, polish_status, polish_by, polished_text)
			values($1,$2,$3,$4,$5,$6,$7,$8)`,
			messageID, d.ReceiverID, d.TargetLang, d.BaseText, d.TranslationStatus, d.PolishStatus, d.PolishBy, d.PolishedText); err != nil {
			return err
		}
	}
	return nil
}

// ViewsOf builds the sender's view (client_msg_id, no delivery) and one view per receiver (own delivery, no client_msg_id).
func (r *MessageRepo) ViewsOf(ctx context.Context, messageID string) (model.MessageViews, error) {
	if !uuidRE.MatchString(messageID) {
		return model.MessageViews{}, model.ErrNotFound
	}
	db := conn(ctx, r.pool)
	base, cmid, err := scanMessage(db.QueryRow(ctx,
		`select `+messageCols+` from messages m join users u on u.id = m.sender_id where m.id = $1`, messageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.MessageViews{}, model.ErrNotFound
	}
	if err != nil {
		return model.MessageViews{}, err
	}
	out := model.MessageViews{Sender: base, Receivers: map[string]model.MessageView{}}
	out.Sender.ClientMsgID = &cmid

	rows, err := db.Query(ctx, `
		select receiver_id::text, id::text, target_lang, base_text, translation_status, polished_text, polish_status
		from deliveries where message_id = $1 and mod_status <> 'block'`, messageID)
	if err != nil {
		return model.MessageViews{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var rid string
		var d model.DeliveryView
		if err := rows.Scan(&rid, &d.ID, &d.TargetLang, &d.BaseText, &d.TranslationStatus, &d.PolishedText, &d.PolishStatus); err != nil {
			return model.MessageViews{}, err
		}
		v := base
		v.Delivery = &d
		out.Receivers[rid] = v
	}
	return out, rows.Err()
}

// ListForViewer returns one page of the viewer's messages in the room, ascending by id.
func (r *MessageRepo) ListForViewer(ctx context.Context, roomID, viewerID string, q model.PageQuery) ([]model.MessageView, bool, error) {
	if !uuidRE.MatchString(roomID) || !uuidRE.MatchString(viewerID) ||
		(q.Before != "" && !uuidRE.MatchString(q.Before)) || (q.After != "" && !uuidRE.MatchString(q.After)) {
		return nil, false, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = model.DefaultPageLimit
	}
	if limit > model.MaxPageLimit {
		limit = model.MaxPageLimit
	}
	cond, order := "", "desc"
	args := []any{roomID, viewerID, limit + 1}
	switch {
	case q.After != "":
		cond, order = " and m.id > $4::uuid", "asc"
		args = append(args, q.After)
	case q.Before != "":
		cond = " and m.id < $4::uuid"
		args = append(args, q.Before)
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select `+messageCols+`, d.id::text, d.target_lang, d.base_text, d.translation_status, d.polished_text, d.polish_status
		from messages m
		join users u on u.id = m.sender_id
		left join deliveries d on d.message_id = m.id and d.receiver_id = $2 and d.mod_status <> 'block'
		where m.room_id = $1 and (m.sender_id = $2 or d.id is not null)`+cond+`
		order by m.id `+order+` limit $3`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var items []model.MessageView
	for rows.Next() {
		var v model.MessageView
		var cmid string
		var did, tl, ts, ps *string
		var base, polished *string
		if err := rows.Scan(&v.ID, &v.RoomID, &v.Sender.UserID, &v.Scope, &v.ToUserID, &v.Kind, &v.PhraseID,
			&v.SrcLang, &v.Text, &cmid, &v.PolishRequested, &v.CreatedAt, &v.Sender.Nickname, &v.Sender.PetType,
			&did, &tl, &base, &ts, &polished, &ps); err != nil {
			return nil, false, err
		}
		if v.Sender.UserID == viewerID {
			v.ClientMsgID = &cmid
		} else if did != nil {
			v.Delivery = &model.DeliveryView{ID: *did, TargetLang: *tl, BaseText: base, TranslationStatus: *ts, PolishedText: polished, PolishStatus: *ps}
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if order == "desc" {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	return items, hasMore, nil
}

// TranslationWork reads the message and its deliveries that are not translated yet.
func (r *MessageRepo) TranslationWork(ctx context.Context, messageID string) (model.TranslationWork, error) {
	if !uuidRE.MatchString(messageID) {
		return model.TranslationWork{}, model.ErrNotFound
	}
	db := conn(ctx, r.pool)
	var w model.TranslationWork
	err := db.QueryRow(ctx, `select id::text, room_id::text, sender_id::text, src_lang, body from messages where id = $1`, messageID).
		Scan(&w.MessageID, &w.RoomID, &w.SenderID, &w.SrcLang, &w.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.TranslationWork{}, model.ErrNotFound
	}
	if err != nil {
		return model.TranslationWork{}, err
	}
	rows, err := db.Query(ctx, `
		select id::text, receiver_id::text, target_lang, polish_status from deliveries
		where message_id = $1 and translation_status <> 'done' order by id`, messageID)
	if err != nil {
		return model.TranslationWork{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p model.PendingDelivery
		if err := rows.Scan(&p.ID, &p.ReceiverID, &p.TargetLang, &p.PolishStatus); err != nil {
			return model.TranslationWork{}, err
		}
		w.Pending = append(w.Pending, p)
	}
	return w, rows.Err()
}

// SetTranslation records a translation result unless the delivery is already done.
func (r *MessageRepo) SetTranslation(ctx context.Context, deliveryID string, baseText *string, status, modStatus string, now time.Time) (bool, error) {
	if !uuidRE.MatchString(deliveryID) {
		return false, nil
	}
	tag, err := conn(ctx, r.pool).Exec(ctx, `
		update deliveries
		set base_text = $2, translation_status = $3, mod_status = $4, translate_attempts = translate_attempts + 1, updated_at = $5
		where id = $1 and translation_status <> 'done'`, deliveryID, baseText, status, modStatus, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StuckTranslations lists messages whose translation was lost: pending too long, or failed
// recently enough to retry and not touched for failedEvery. Oldest first.
func (r *MessageRepo) StuckTranslations(ctx context.Context, now time.Time, pendingAfter, failedEvery, giveUpAfter time.Duration, limit int) ([]string, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select m.id::text
		from messages m join deliveries d on d.message_id = m.id
		where (d.translation_status = 'pending' and d.updated_at < $1::timestamptz - $2::interval)
		   or (d.translation_status = 'failed' and m.created_at > $1::timestamptz - $4::interval
		       and d.updated_at < $1::timestamptz - $3::interval)
		group by m.id, m.created_at
		order by m.created_at, m.id
		limit $5`, now, pendingAfter, failedEvery, giveUpAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PolishWork reads one delivery with its message and the receiver's MBTI.
func (r *MessageRepo) PolishWork(ctx context.Context, deliveryID string) (model.PolishWork, error) {
	if !uuidRE.MatchString(deliveryID) {
		return model.PolishWork{}, model.ErrNotFound
	}
	var w model.PolishWork
	err := conn(ctx, r.pool).QueryRow(ctx, `
		select d.id::text, m.id::text, m.room_id::text, m.sender_id::text, d.receiver_id::text, m.kind, m.src_lang, m.body,
		       d.target_lang, d.base_text, d.translation_status, d.polish_status, d.polish_by::text, coalesce(u.mbti, ''), d.mod_status
		from deliveries d join messages m on m.id = d.message_id join users u on u.id = d.receiver_id
		where d.id = $1`, deliveryID).
		Scan(&w.DeliveryID, &w.MessageID, &w.RoomID, &w.SenderID, &w.ReceiverID, &w.Kind, &w.SrcLang, &w.Body,
			&w.TargetLang, &w.BaseText, &w.TranslationStatus, &w.PolishStatus, &w.PolishBy, &w.ReceiverMBTI, &w.ModStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.PolishWork{}, model.ErrNotFound
	}
	return w, err
}

// WithdrawMessage hides a message from every receiver in one statement: the message and all its
// deliveries get mod_status 'block', unfinished translations are marked done (so the sweep skips them)
// and pending polishes fall back. It returns every receiver id.
func (r *MessageRepo) WithdrawMessage(ctx context.Context, messageID string, now time.Time) ([]string, error) {
	if !uuidRE.MatchString(messageID) {
		return nil, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `
		with m as (
			update messages set mod_status = 'block' where id = $1 returning id
		)
		update deliveries d
		set mod_status = 'block',
		    translation_status = 'done',
		    polish_status = case when d.polish_status = 'pending' then 'fallback' else d.polish_status end,
		    updated_at = $2
		from m
		where d.message_id = m.id
		returning d.receiver_id::text`, messageID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetPolish records a polish result while the row is still pending.
func (r *MessageRepo) SetPolish(ctx context.Context, deliveryID string, polished *string, status string, now time.Time) (bool, error) {
	if !uuidRE.MatchString(deliveryID) {
		return false, nil
	}
	tag, err := conn(ctx, r.pool).Exec(ctx, `
		update deliveries set polished_text = $2, polish_status = $3, updated_at = $4
		where id = $1 and polish_status = 'pending'`, deliveryID, polished, status, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimPolish moves the receiver's own row from none to pending.
func (r *MessageRepo) ClaimPolish(ctx context.Context, deliveryID, receiverID string, now time.Time) (bool, error) {
	if !uuidRE.MatchString(deliveryID) || !uuidRE.MatchString(receiverID) {
		return false, nil
	}
	tag, err := conn(ctx, r.pool).Exec(ctx, `
		update deliveries set polish_status = 'pending', polish_by = $2, updated_at = $3
		where id = $1 and receiver_id = $2 and polish_status = 'none'`, deliveryID, receiverID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StuckPolishes lists pending polishes whose translation is done and that were not touched for pendingAfter.
func (r *MessageRepo) StuckPolishes(ctx context.Context, now time.Time, pendingAfter time.Duration, limit int) ([]string, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select id::text from deliveries
		where polish_status = 'pending' and translation_status = 'done' and updated_at < $1::timestamptz - $2::interval
		order by updated_at, id
		limit $3`, now, pendingAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GiveUpPolishes sets fallback on pending polishes whose translation failed for good.
func (r *MessageRepo) GiveUpPolishes(ctx context.Context, now time.Time, giveUpAfter time.Duration, limit int) ([]model.PolishGiveUp, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `
		with picked as (
			select d.id from deliveries d join messages m on m.id = d.message_id
			where d.polish_status = 'pending' and d.translation_status = 'failed'
			  and m.created_at < $1::timestamptz - $2::interval
			order by m.created_at, d.id
			limit $3
			for update of d skip locked
		)
		update deliveries d set polish_status = 'fallback', updated_at = $1
		from picked, messages m
		where d.id = picked.id and m.id = d.message_id
		returning d.id::text, m.id::text, m.room_id::text, d.receiver_id::text`, now, giveUpAfter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PolishGiveUp
	for rows.Next() {
		var g model.PolishGiveUp
		if err := rows.Scan(&g.DeliveryID, &g.MessageID, &g.RoomID, &g.ReceiverID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
