package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/realtime"
)

// pushJob es el payload de send_push.
type pushJob struct {
	NotificationID uuid.UUID `json:"notification_id"`
}

// Notify guarda el aviso de cada destinatario con la transaccion q (con el
// schema de la empresa) y encola el push y el correo segun sus preferencias.
func (s *Service) Notify(ctx context.Context, q database.Querier, tenantID uuid.UUID, n Notice) error {
	recipients := make([]uuid.UUID, 0, len(n.Recipients))
	for _, r := range n.Recipients {
		if r == uuid.Nil || (n.Except != nil && r == *n.Except) || slices.Contains(recipients, r) {
			continue
		}
		recipients = append(recipients, r)
	}
	if len(recipients) == 0 {
		return nil
	}
	data := map[string]any{}
	for k, v := range n.Data {
		data[k] = v
	}
	if n.Route != "" {
		data["route"] = n.Route
	}
	if n.AppRoute != "" {
		data["app_route"] = n.AppRoute
	}
	wantPush, wantEmail := slices.Contains(n.Channels, ChannelPush), slices.Contains(n.Channels, ChannelEmail)

	prefs, err := preferencesFor(ctx, q, n.Type, recipients)
	if err != nil {
		return err
	}
	var people map[uuid.UUID][2]string
	var tenantName string
	if wantEmail {
		if people, tenantName, err = contacts(ctx, q, tenantID, recipients); err != nil {
			return err
		}
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	critical := Critical[n.Type]
	for _, user := range recipients {
		p, ok := prefs[user]
		push := wantPush && (critical || !ok || p[0])
		email := wantEmail && (critical || !ok || p[1])
		channels := []string{ChannelInApp}
		if push {
			channels = append(channels, ChannelPush)
		}
		if email {
			channels = append(channels, ChannelEmail)
		}
		rawChannels, _ := json.Marshal(channels)
		var id uuid.UUID
		var created time.Time
		if err := q.QueryRow(ctx, `
			INSERT INTO notifications (recipient_user_id, type, title, body, data, channels, sent_email_at)
			VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $7::bool THEN now() END)
			RETURNING id, created_at`,
			user, n.Type, truncate(n.Title, 160), n.Body, rawData, rawChannels, email,
		).Scan(&id, &created); err != nil {
			return fmt.Errorf("guardando el aviso: %w", err)
		}
		if push {
			if err := jobs.Enqueue(ctx, q, jobs.Job{
				Type: JobSendPush, TenantID: &tenantID, Payload: pushJob{NotificationID: id}, MaxAttempts: 3,
			}); err != nil {
				return err
			}
		}
		if email {
			person := people[user]
			if person[0] != "" {
				if err := jobs.EnqueueEmail(ctx, q, &tenantID, jobs.Email{
					To: person[0], Template: "notification",
					Data: map[string]any{
						"Name": firstName(person[1]), "TenantName": tenantName,
						"Title": n.Title, "Body": n.Body, "Link": s.link(n.Route),
					},
				}); err != nil {
					return err
				}
			}
		}
		if err := realtime.Publish(ctx, q, tenantID, []uuid.UUID{user}, "notification", NotificationRead{
			ID: id, Type: n.Type, Title: n.Title, Body: n.Body, Data: data, CreatedAt: created,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) link(route string) string {
	if route == "" {
		return strings.TrimRight(s.webBase, "/")
	}
	return strings.TrimRight(s.webBase, "/") + route
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func firstName(full string) string {
	if i := strings.IndexByte(strings.TrimSpace(full), ' '); i > 0 {
		return full[:i]
	}
	return full
}

// preferencesFor devuelve [push, email] de quienes tienen fila para el tipo.
func preferencesFor(ctx context.Context, q database.Querier, typ string, users []uuid.UUID) (map[uuid.UUID][2]bool, error) {
	rows, err := q.Query(ctx, `SELECT user_id, push, email FROM notification_preferences WHERE type = $1 AND user_id = ANY($2)`, typ, users)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][2]bool{}
	for rows.Next() {
		var id uuid.UUID
		var push, email bool
		if err := rows.Scan(&id, &push, &email); err != nil {
			return nil, err
		}
		out[id] = [2]bool{push, email}
	}
	return out, rows.Err()
}

func contacts(ctx context.Context, q database.Querier, tenantID uuid.UUID, users []uuid.UUID) (map[uuid.UUID][2]string, string, error) {
	rows, err := q.Query(ctx, `SELECT id, email, full_name FROM shared.users WHERE id = ANY($1) AND is_active`, users)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := map[uuid.UUID][2]string{}
	for rows.Next() {
		var id uuid.UUID
		var email, name string
		if err := rows.Scan(&id, &email, &name); err != nil {
			return nil, "", err
		}
		out[id] = [2]string{email, name}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var tenant string
	if err := q.QueryRow(ctx, `SELECT name FROM shared.tenants WHERE id = $1`, tenantID).Scan(&tenant); err != nil {
		return nil, "", err
	}
	return out, tenant, nil
}

// Holders son los usuarios activos de la empresa con alguno de los permisos.
// Con propertyID, los roles con alcance restringido solo cuentan si tienen
// asignada esa propiedad.
func Holders(ctx context.Context, q database.Querier, tenantID uuid.UUID, propertyID *uuid.UUID, perms ...authz.Permission) ([]uuid.UUID, error) {
	names := make([]string, len(perms))
	for i, p := range perms {
		names[i] = string(p)
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT m.user_id
		FROM shared.memberships m
		JOIN shared.roles r ON r.id = m.role_id
		JOIN shared.role_permissions rp ON rp.role_id = r.id AND rp.permission = ANY($2)
		JOIN shared.users u ON u.id = m.user_id AND u.is_active
		WHERE m.tenant_id = $1 AND m.status = 'active'
		  AND (NOT r.scoped OR $3::uuid IS NULL OR EXISTS (
		       SELECT 1 FROM property_assignments pa WHERE pa.user_id = m.user_id AND pa.property_id = $3))`,
		tenantID, names, propertyID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ── Bandeja ────────────────────────────────────────────────

// List devuelve los avisos del usuario, los mas nuevos primero.
func (s *Service) List(ctx context.Context, actor authz.Actor, unreadOnly bool, cursor string, limit int) (NotificationPage, error) {
	limit = paging.Limit(limit)
	key, err := paging.Decode(cursor, 2)
	if err != nil {
		return NotificationPage{}, err
	}
	var afterAt, afterID *string
	if key != nil {
		afterAt, afterID = &key[0], &key[1]
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (NotificationPage, error) {
		rows, err := q.Query(ctx, `
			SELECT id, type, title, body, data, read_at, created_at FROM notifications
			WHERE recipient_user_id = $1 AND (NOT $2::bool OR read_at IS NULL)
			  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
			ORDER BY created_at DESC, id DESC LIMIT $5`,
			actor.UserID, unreadOnly, afterAt, afterID, limit+1)
		if err != nil {
			return NotificationPage{}, err
		}
		items, err := pgx.CollectRows(rows, pgx.RowToStructByName[NotificationRead])
		if err != nil {
			return NotificationPage{}, err
		}
		var unread int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE recipient_user_id = $1 AND read_at IS NULL`,
			actor.UserID).Scan(&unread); err != nil {
			return NotificationPage{}, err
		}
		items, next := paging.Page(items, limit, func(n NotificationRead) []string {
			return []string{n.CreatedAt.Format(time.RFC3339Nano), n.ID.String()}
		})
		return NotificationPage{Items: items, NextCursor: next, Unread: unread}, nil
	})
}

// MarkRead marca un aviso propio como leido (repetirlo no cambia nada).
func (s *Service) MarkRead(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		tag, err := q.Exec(ctx, `UPDATE notifications SET read_at = coalesce(read_at, now()) WHERE id = $1 AND recipient_user_id = $2`,
			id, actor.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return realtime.Publish(ctx, q, actor.TenantID, []uuid.UUID{actor.UserID}, "notification.read", map[string]any{"id": id})
	})
}

// MarkAllRead marca todos los avisos propios como leidos.
func (s *Service) MarkAllRead(ctx context.Context, actor authz.Actor) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		if _, err := q.Exec(ctx, `UPDATE notifications SET read_at = now() WHERE recipient_user_id = $1 AND read_at IS NULL`,
			actor.UserID); err != nil {
			return err
		}
		return realtime.Publish(ctx, q, actor.TenantID, []uuid.UUID{actor.UserID}, "notification.read", map[string]any{"all": true})
	})
}

// Preferences devuelve todos los tipos con lo que eligio el usuario.
func (s *Service) Preferences(ctx context.Context, actor authz.Actor) ([]Preference, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Preference, error) {
		types := make([]string, 0, len(Labels))
		for t := range Labels {
			types = append(types, t)
		}
		slices.Sort(types)
		prefs := map[string][2]bool{}
		rows, err := q.Query(ctx, `SELECT type, push, email FROM notification_preferences WHERE user_id = $1`, actor.UserID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var t string
			var push, email bool
			if err := rows.Scan(&t, &push, &email); err != nil {
				return nil, err
			}
			prefs[t] = [2]bool{push, email}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out := make([]Preference, 0, len(types))
		for _, t := range types {
			p, ok := prefs[t]
			if !ok || Critical[t] {
				p = [2]bool{true, true}
			}
			out = append(out, Preference{Type: t, Label: Labels[t], Push: p[0], Email: p[1], Critical: Critical[t]})
		}
		return out, nil
	})
}

// SetPreferences guarda lo elegido (los criticos se ignoran).
func (s *Service) SetPreferences(ctx context.Context, actor authz.Actor, in PreferencesUpdate) ([]Preference, error) {
	for i, p := range in.Items {
		if _, ok := Labels[p.Type]; !ok {
			return nil, ErrInvalidType.WithField(fmt.Sprintf("body.items[%d].type", i))
		}
	}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		for _, p := range in.Items {
			if Critical[p.Type] {
				continue
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO notification_preferences (user_id, type, push, email) VALUES ($1, $2, $3, $4)
				ON CONFLICT (user_id, type) DO UPDATE SET push = EXCLUDED.push, email = EXCLUDED.email, updated_at = now()`,
				actor.UserID, p.Type, p.Push, p.Email); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Preferences(ctx, actor)
}

// RegisterDevice guarda (o mueve a este usuario) el token FCM del telefono.
func (s *Service) RegisterDevice(ctx context.Context, actor authz.Actor, in DeviceRegister) error {
	token := strings.TrimSpace(in.Token)
	if token == "" {
		return ErrTokenEmpty.WithField("body.token")
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `
			INSERT INTO shared.device_tokens (user_id, fcm_token, platform) VALUES ($1, $2, $3)
			ON CONFLICT (fcm_token) DO UPDATE SET user_id = EXCLUDED.user_id, platform = EXCLUDED.platform,
			       last_seen_at = now(), updated_at = now()`,
			actor.UserID, token, in.Platform)
		return err
	})
}

// RemoveDevice borra el token (al cerrar sesion). Solo el propio.
func (s *Service) RemoveDevice(ctx context.Context, actor authz.Actor, token string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		tag, err := q.Exec(ctx, `DELETE FROM shared.device_tokens WHERE fcm_token = $1 AND user_id = $2`, token, actor.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDeviceNotFound
		}
		return nil
	})
}

// GuestPush es un push al telefono de un huesped (guest_access_tokens.fcm_token).
type GuestPush struct {
	Token string            `json:"token"`
	Title string            `json:"title"`
	Body  string            `json:"body"`
	Data  map[string]string `json:"data"`
}

// EnqueueGuestPush encola el push de un huesped con la transaccion q. Los
// huespedes no tienen bandeja: solo push (y el correo que mande quien llama).
func EnqueueGuestPush(ctx context.Context, q database.Querier, tenantID uuid.UUID, m GuestPush) error {
	if m.Token == "" {
		return nil
	}
	return jobs.Enqueue(ctx, q, jobs.Job{Type: JobSendGuestPush, TenantID: &tenantID, Payload: m, MaxAttempts: 3})
}

// UserPush es un push a los telefonos de un usuario sin aviso en la bandeja
// (los mensajes del chat ya se ven en su conversacion).
type UserPush struct {
	UserID uuid.UUID         `json:"user_id"`
	Type   string            `json:"type"`
	Title  string            `json:"title"`
	Body   string            `json:"body"`
	Data   map[string]string `json:"data"`
}

// EnqueueUserPush encola el push con la transaccion q si el usuario no lo
// apago en sus preferencias (tipo m.Type).
func EnqueueUserPush(ctx context.Context, q database.Querier, tenantID uuid.UUID, m UserPush) error {
	if m.UserID == uuid.Nil {
		return nil
	}
	if !Critical[m.Type] {
		prefs, err := preferencesFor(ctx, q, m.Type, []uuid.UUID{m.UserID})
		if err != nil {
			return err
		}
		if p, ok := prefs[m.UserID]; ok && !p[0] {
			return nil
		}
	}
	m.Title = truncate(m.Title, 160)
	m.Body = truncate(m.Body, 240)
	return jobs.Enqueue(ctx, q, jobs.Job{Type: JobSendUserPush, TenantID: &tenantID, Payload: m, MaxAttempts: 3})
}

func (s *Service) sendUserPush(ctx context.Context, m UserPush) error {
	tokens, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]string, error) {
		rows, err := q.Query(ctx, `SELECT fcm_token FROM shared.device_tokens WHERE user_id = $1 ORDER BY last_seen_at DESC LIMIT 10`, m.UserID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	})
	if err != nil {
		return err
	}
	data := map[string]string{"type": m.Type}
	for k, v := range m.Data {
		data[k] = v
	}
	var dead []string
	var lastErr error
	for _, t := range tokens {
		err := s.push.Send(ctx, PushMessage{Token: t, Title: m.Title, Body: m.Body, Data: data})
		switch {
		case errors.Is(err, ErrInvalidToken):
			dead = append(dead, t)
		case err != nil:
			lastErr = err
		}
	}
	if len(dead) > 0 {
		if err := s.db.InTx(ctx, func(q database.Querier) error {
			_, err := q.Exec(ctx, `DELETE FROM shared.device_tokens WHERE fcm_token = ANY($1)`, dead)
			return err
		}); err != nil {
			return err
		}
	}
	return lastErr
}

// ── Trabajos ──────────────────────────────────────────────

// HandleJob corre send_push y task_reminders.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	switch job.Type {
	case JobSendPush:
		var p pushJob
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("payload de push invalido: %w", err)
		}
		return s.sendPush(ctx, p.NotificationID)
	case JobSendGuestPush:
		var m GuestPush
		if err := json.Unmarshal(job.Payload, &m); err != nil {
			return fmt.Errorf("payload de push invalido: %w", err)
		}
		err := s.push.Send(ctx, PushMessage{Token: m.Token, Title: m.Title, Body: m.Body, Data: m.Data})
		if errors.Is(err, ErrInvalidToken) {
			return nil // el huesped desinstalo la app: no se reintenta
		}
		return err
	case JobSendUserPush:
		var m UserPush
		if err := json.Unmarshal(job.Payload, &m); err != nil {
			return fmt.Errorf("payload de push invalido: %w", err)
		}
		return s.sendUserPush(ctx, m)
	case JobTaskReminders:
		if job.TenantID == nil {
			return s.fanOut(ctx, job.Type)
		}
		return s.db.InTx(ctx, func(q database.Querier) error {
			_, err := s.TaskReminders(ctx, q, *job.TenantID)
			return err
		})
	}
	return fmt.Errorf("trabajo desconocido %s", job.Type)
}

func (s *Service) fanOut(ctx context.Context, jobType string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := jobs.Enqueue(ctx, q, jobs.Job{
				Type: jobType, TenantID: &id, DedupeKey: jobType + ":" + id.String(), MaxAttempts: 3,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) sendPush(ctx context.Context, id uuid.UUID) error {
	type row struct {
		User  uuid.UUID
		Type  string
		Title string
		Body  string
		Data  map[string]any
	}
	var n row
	var tokens []string
	err := s.db.InTx(ctx, func(q database.Querier) error {
		err := q.QueryRow(ctx, `SELECT recipient_user_id, type, title, body, data FROM notifications WHERE id = $1 AND sent_push_at IS NULL`, id).
			Scan(&n.User, &n.Type, &n.Title, &n.Body, &n.Data)
		if err != nil {
			return err
		}
		rows, err := q.Query(ctx, `SELECT fcm_token FROM shared.device_tokens WHERE user_id = $1 ORDER BY last_seen_at DESC LIMIT 10`, n.User)
		if err != nil {
			return err
		}
		tokens, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // ya se mando o el aviso ya no existe
	}
	if err != nil {
		return err
	}
	data := map[string]string{"notification_id": id.String(), "type": n.Type}
	for k, v := range n.Data {
		if str, ok := v.(string); ok {
			data[k] = str
		}
	}
	var dead []string
	var lastErr error
	for _, t := range tokens {
		err := s.push.Send(ctx, PushMessage{Token: t, Title: n.Title, Body: n.Body, Data: data})
		switch {
		case errors.Is(err, ErrInvalidToken):
			dead = append(dead, t)
		case err != nil:
			lastErr = err
		}
	}
	err = s.db.InTx(ctx, func(q database.Querier) error {
		if len(dead) > 0 {
			if _, err := q.Exec(ctx, `DELETE FROM shared.device_tokens WHERE fcm_token = ANY($1)`, dead); err != nil {
				return err
			}
		}
		if lastErr != nil {
			return nil
		}
		_, err := q.Exec(ctx, `UPDATE notifications SET sent_push_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return err
	}
	return lastErr
}

// TaskReminders avisa a quien tiene una tarea pendiente que empieza en los
// proximos 30 minutos (hora local de la empresa), una sola vez por tarea.
func (s *Service) TaskReminders(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	rows, err := q.Query(ctx, `
		WITH tz AS (SELECT timezone FROM shared.tenants WHERE id = $1)
		SELECT t.id, t.assigned_to, p.code, p.name, to_char(t.window_start, 'HH24:MI')
		FROM tasks t JOIN properties p ON p.id = t.property_id, tz
		WHERE t.status = 'pending' AND t.assigned_to IS NOT NULL AND t.window_start IS NOT NULL
		  AND (t.scheduled_date + t.window_start) AT TIME ZONE tz.timezone BETWEEN $2::timestamptz AND $2::timestamptz + interval '30 minutes'
		  AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.type = 'task_reminder' AND n.data->>'task_id' = t.id::text)`,
		tenantID, s.now())
	if err != nil {
		return 0, err
	}
	type due struct {
		ID         uuid.UUID
		User       uuid.UUID
		Code, Name string
		Start      string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.ID, &d.User, &d.Code, &d.Name, &d.Start)
		return d, err
	})
	if err != nil {
		return 0, err
	}
	for _, d := range list {
		if err := s.Notify(ctx, q, tenantID, Notice{
			Recipients: []uuid.UUID{d.User}, Type: TypeTaskReminder,
			Title: "Tu siguiente tarea empieza pronto",
			Body:  fmt.Sprintf("%s · %s a las %s", d.Code, d.Name, d.Start),
			Route: "/itinerario?tarea=" + d.ID.String(), AppRoute: "/tasks/" + d.ID.String(),
			Data:     map[string]any{"task_id": d.ID.String()},
			Channels: []string{ChannelPush},
		}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}
