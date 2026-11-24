package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const usage = `usage:
  modctl flags [-status open] [-stage S] [-since 24h]
  modctl flags ack <id>...
  modctl reports [-status open]
  modctl reports resolve <id> [-note N]
  modctl reports dismiss <id> [-note N]
  modctl ban <user_id>
  modctl unban <user_id>`

func usageErr(msg string) error {
	if msg == "" {
		return errors.New(usage)
	}
	return fmt.Errorf("%s\n%s", msg, usage)
}

// Run executes one modctl command and writes its result to out.
func Run(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, now func() time.Time) error {
	if len(args) == 0 {
		return usageErr("")
	}
	rest := args[1:]
	switch args[0] {
	case "flags":
		if len(rest) > 0 && rest[0] == "ack" {
			return ackFlags(ctx, pool, rest[1:], out)
		}
		return listFlags(ctx, pool, rest, out, now)
	case "reports":
		if len(rest) > 0 && (rest[0] == "resolve" || rest[0] == "dismiss") {
			return handleReport(ctx, pool, rest[0], rest[1:], out, now)
		}
		return listReports(ctx, pool, rest, out)
	case "ban":
		return setBan(ctx, pool, true, rest, out, now)
	case "unban":
		return setBan(ctx, pool, false, rest, out, now)
	}
	return usageErr("unknown command " + args[0])
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
}

func listFlags(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer, now func() time.Time) error {
	fs := newFlagSet("flags")
	status := fs.String("status", "open", "open|reviewed")
	stage := fs.String("stage", "", "outgoing|translation|polish|nickname")
	since := fs.Duration("since", 0, "only flags newer than this")
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected argument " + fs.Arg(0))
	}
	var cutoff *time.Time
	if *since > 0 {
		c := now().Add(-*since)
		cutoff = &c
	}
	rows, err := pool.Query(ctx, `
		select f.id::text, f.created_at, f.stage, f.verdict, f.source, f.terms, f.user_id::text,
		       coalesce(f.text, d.base_text, m.body, '')
		from mod_flags f
		left join messages m on m.id = f.message_id
		left join deliveries d on d.id = f.delivery_id
		where f.status = $1 and ($2 = '' or f.stage = $2) and ($3::timestamptz is null or f.created_at >= $3)
		order by f.created_at desc`, *status, *stage, cutoff)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, st, verdict, source, user, text string
		var at time.Time
		var terms []string
		if err := rows.Scan(&id, &at, &st, &verdict, &source, &terms, &user, &text); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\tuser=%s\tterms=%s\t%s\n", id, at.UTC().Format(time.RFC3339), st, verdict, source, user,
			strings.Join(terms, ","), oneLine(text))
	}
	return rows.Err()
}

func ackFlags(ctx context.Context, pool *pgxpool.Pool, ids []string, out io.Writer) error {
	if len(ids) == 0 {
		return usageErr("flags ack needs at least one id")
	}
	for _, id := range ids {
		tag, err := pool.Exec(ctx, `update mod_flags set status='reviewed' where id::text = $1`, id)
		if err != nil {
			return fmt.Errorf("ack %s: %w", id, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("flag %s not found", id)
		}
		fmt.Fprintf(out, "acked %s\n", id)
	}
	return nil
}

func listReports(ctx context.Context, pool *pgxpool.Pool, args []string, out io.Writer) error {
	fs := newFlagSet("reports")
	status := fs.String("status", "open", "open|resolved|dismissed")
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	if fs.NArg() > 0 {
		return usageErr("unexpected argument " + fs.Arg(0))
	}
	rows, err := pool.Query(ctx, `
		select r.id::text, r.created_at, r.reason, coalesce(r.note,''),
		       coalesce(rp.nickname,''), coalesce(rp.age_band,''),
		       coalesce(t.nickname,''), coalesce(t.age_band,''),
		       coalesce(m.body,''), coalesce(d.base_text,'')
		from reports r
		join users rp on rp.id = r.reporter_id
		join users t on t.id = r.target_id
		left join messages m on m.id = r.message_id
		left join deliveries d on d.message_id = r.message_id and d.receiver_id = r.reporter_id
		where r.status = $1
		order by (r.reason = 'minor_safety' or rp.age_band = '13_17' or t.age_band = '13_17') desc, r.created_at desc`, *status)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, reason, note, rn, ra, tn, ta, body, base string
		var at time.Time
		if err := rows.Scan(&id, &at, &reason, &note, &rn, &ra, &tn, &ta, &body, &base); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\t%s\t%s\treporter=%s(%s)\ttarget=%s(%s)", id, at.UTC().Format(time.RFC3339), reason, oneLine(rn), ra, oneLine(tn), ta)
		if note != "" {
			fmt.Fprintf(out, "\tnote=%s", oneLine(note))
		}
		if body != "" {
			fmt.Fprintf(out, "\tmessage=%s\treporter_saw=%s", oneLine(body), oneLine(base))
		}
		fmt.Fprintln(out)
	}
	return rows.Err()
}

func handleReport(ctx context.Context, pool *pgxpool.Pool, action string, args []string, out io.Writer, now func() time.Time) error {
	fs := newFlagSet("reports " + action)
	note := fs.String("note", "", "handling note")
	// Accept the id before or after -note.
	var ids []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return usageErr(err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(ids) != 1 {
		return usageErr("reports " + action + " needs exactly one id")
	}
	status := map[string]string{"resolve": "resolved", "dismiss": "dismissed"}[action]
	tag, err := pool.Exec(ctx, `update reports set status=$2, handled_at=$3, handled_note=$4
		where id::text=$1 and status='open'`, ids[0], status, now(), *note)
	if err != nil {
		return fmt.Errorf("%s %s: %w", action, ids[0], err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("report %s not found or not open", ids[0])
	}
	fmt.Fprintf(out, "%s %s\n", status, ids[0])
	return nil
}

func setBan(ctx context.Context, pool *pgxpool.Pool, ban bool, args []string, out io.Writer, now func() time.Time) error {
	name, from, to := "unban", "banned", "active"
	if ban {
		name, from, to = "ban", "active", "banned"
	}
	if len(args) != 1 {
		return usageErr(name + " needs exactly one user id")
	}
	id := args[0]
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var cur string
	if err := tx.QueryRow(ctx, `select status from users where id::text=$1 for update`, id).Scan(&cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("user %s not found", id)
		}
		return err
	}
	if cur != from {
		return fmt.Errorf("cannot %s user %s: status is %s, want %s", name, id, cur, from)
	}
	if _, err := tx.Exec(ctx, `update users set status=$2 where id::text=$1`, id, to); err != nil {
		return err
	}
	if ban {
		if _, err := tx.Exec(ctx, `update refresh_tokens set revoked_at=$2 where user_id::text=$1 and revoked_at is null`, id, now()); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s ok %s\n", name, id)
	return nil
}
