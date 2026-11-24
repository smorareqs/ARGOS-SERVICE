package reservations

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

const selectChannel = `
	SELECT c.id, c.name, c.kind, c.requires_deposit, c.has_checkin, c.default_deposit_amount::float8,
	       c.is_active, c.created_at, c.updated_at,
	       (SELECT count(*)::int FROM property_ical_feeds f WHERE f.channel_id = c.id AND f.is_active) AS feeds_count,
	       (SELECT count(*)::int FROM reservations r WHERE r.channel_id = c.id
	         AND r.status IN ('pending', 'confirmed', 'checked_in')) AS upcoming_count
	FROM booking_channels c`

// Channels lista los canales: activos primero y por nombre.
func (s *Service) Channels(ctx context.Context, _ authz.Actor) ([]ChannelRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]ChannelRead, error) {
		rows, err := q.Query(ctx, selectChannel+` ORDER BY NOT c.is_active, c.kind = 'direct' DESC, lower(c.name)`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[ChannelRead])
	})
}

// CreateChannel da de alta un canal (agencia, otra OTA).
func (s *Service) CreateChannel(ctx context.Context, actor authz.Actor, in ChannelCreate) (ChannelRead, error) {
	name := strings.TrimSpace(in.Name)
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ChannelRead, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO booking_channels (name, kind, requires_deposit, has_checkin, default_deposit_amount)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			name, in.Kind, in.RequiresDeposit, in.HasCheckin, in.DefaultDepositAmount).Scan(&id)
		if uniqueViolation(err, "uq_booking_channels_name") {
			return ChannelRead{}, ErrChannelNameTaken.WithField("body.name")
		}
		if err != nil {
			return ChannelRead{}, err
		}
		c, err := channelByID(ctx, q, id)
		if err != nil {
			return ChannelRead{}, err
		}
		return c, audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "channel.created", Entity: "booking_channel", EntityID: &id,
			After: map[string]any{"name": c.Name, "kind": c.Kind, "requires_deposit": c.RequiresDeposit, "has_checkin": c.HasCheckin},
		})
	})
}

// UpdateChannel cambia las reglas del canal. Las reservas existentes conservan
// su deposito; las nuevas toman las reglas nuevas.
func (s *Service) UpdateChannel(ctx context.Context, actor authz.Actor, id uuid.UUID, in ChannelUpdate) (ChannelRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ChannelRead, error) {
		cur, err := channelByID(ctx, q, id)
		if err != nil {
			return ChannelRead{}, err
		}
		v, fields := mergeChannel(cur, in)
		if len(fields) == 0 {
			return cur, nil
		}
		_, err = q.Exec(ctx, `
			UPDATE booking_channels SET name = $2, kind = $3, requires_deposit = $4, has_checkin = $5,
			       default_deposit_amount = $6, is_active = $7, updated_at = now()
			WHERE id = $1`, id, v.Name, v.Kind, v.RequiresDeposit, v.HasCheckin, v.DefaultDepositAmount, v.IsActive)
		if uniqueViolation(err, "uq_booking_channels_name") {
			return ChannelRead{}, ErrChannelNameTaken.WithField("body.name")
		}
		if err != nil {
			return ChannelRead{}, err
		}
		out, err := channelByID(ctx, q, id)
		if err != nil {
			return ChannelRead{}, err
		}
		return out, audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "channel.updated", Entity: "booking_channel", EntityID: &id,
			After: map[string]any{"fields": fields},
		})
	})
}

func mergeChannel(cur ChannelRead, in ChannelUpdate) (ChannelRead, []string) {
	fields := []string{}
	if in.Name != nil && strings.TrimSpace(*in.Name) != cur.Name {
		cur.Name, fields = strings.TrimSpace(*in.Name), append(fields, "name")
	}
	if in.Kind != nil && *in.Kind != cur.Kind {
		cur.Kind, fields = *in.Kind, append(fields, "kind")
	}
	if in.RequiresDeposit != nil && *in.RequiresDeposit != cur.RequiresDeposit {
		cur.RequiresDeposit, fields = *in.RequiresDeposit, append(fields, "requires_deposit")
	}
	if in.HasCheckin != nil && *in.HasCheckin != cur.HasCheckin {
		cur.HasCheckin, fields = *in.HasCheckin, append(fields, "has_checkin")
	}
	if in.DefaultDepositAmount.Sent {
		cur.DefaultDepositAmount, fields = optional.Apply(cur.DefaultDepositAmount, in.DefaultDepositAmount),
			append(fields, "default_deposit_amount")
	}
	if in.IsActive != nil && *in.IsActive != cur.IsActive {
		cur.IsActive, fields = *in.IsActive, append(fields, "is_active")
	}
	return cur, fields
}

func channelByID(ctx context.Context, q database.Querier, id uuid.UUID) (ChannelRead, error) {
	rows, err := q.Query(ctx, selectChannel+` WHERE c.id = $1`, id)
	if err != nil {
		return ChannelRead{}, err
	}
	c, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[ChannelRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return ChannelRead{}, ErrChannelNotFound
	}
	return c, err
}

// activeChannel valida un canal para reservas nuevas (422 si no sirve).
func activeChannel(ctx context.Context, q database.Querier, id uuid.UUID, field string) (ChannelRead, error) {
	c, err := channelByID(ctx, q, id)
	if errors.Is(err, ErrChannelNotFound) || (err == nil && !c.IsActive) {
		return ChannelRead{}, ErrUnknownChannel.WithField(field)
	}
	return c, err
}

// uniqueViolation indica si err viola la restriccion unica indicada.
func uniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}

// overlapViolation indica que la base rechazo dos estancias que se cruzan.
func overlapViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23P01"
}
