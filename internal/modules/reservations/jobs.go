package reservations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Trabajos del modulo (los registra app.NewJobRegistry). Los recurrentes
// corren sin empresa y encolan uno por empresa activa con la bandera
// reservations; ese corre en el schema de la empresa.
const (
	// JobIcalSync lee los calendarios iCal (cada 30 minutos).
	JobIcalSync = "ical_sync"
	// JobStatus marca entradas y salidas de canales sin check-in formal (cada hora).
	JobStatus = "reservation_status"
)

// HandleJob ejecuta los trabajos del modulo.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.TenantID == nil {
		return s.fanOut(ctx, job.Type)
	}
	switch job.Type {
	case JobIcalSync:
		res, err := s.RunIcalSync(ctx, *job.TenantID)
		for _, r := range res {
			if !r.OK {
				s.log.WarnContext(ctx, "Calendario iCal con error", "feed_id", r.FeedID, "error", *r.Error)
			}
		}
		return err
	case JobStatus:
		return s.db.InTx(ctx, func(q database.Querier) error {
			_, err := s.RunAutoStatus(ctx, q, *job.TenantID)
			return err
		})
	case JobReminder:
		var p reminderPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return err
		}
		return s.db.InTx(ctx, func(q database.Querier) error {
			_, err := s.RunReminder(ctx, q, p)
			return err
		})
	}
	return fmt.Errorf("trabajo desconocido %s", job.Type)
}

// fanOut encola el trabajo para cada empresa activa con reservas.
func (s *Service) fanOut(ctx context.Context, jobType string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `
			SELECT id FROM shared.tenants
			WHERE is_active AND coalesce((features->>'reservations')::bool, false) ORDER BY slug`)
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

// --- Inicio -----------------------------------------------------------------------------

// TodaySummary cuenta llegadas, salidas y estancias de hoy en el alcance (Inicio
// y encabezado de Reservas).
func TodaySummary(ctx context.Context, q database.Querier, scope authz.PropertyScope, today string) (Today, error) {
	all, ids := scope.SQLArgs()
	out := Today{Available: true}
	err := q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE r.check_in_date = $3::date AND r.status IN ('confirmed', 'checked_in'))::int,
		       count(*) FILTER (WHERE r.check_out_date = $3::date AND r.status IN ('confirmed', 'checked_in', 'checked_out'))::int,
		       count(*) FILTER (WHERE r.status = 'checked_in'
		                          OR (r.status = 'confirmed' AND r.check_in_date < $3::date AND r.check_out_date > $3::date))::int,
		       count(*) FILTER (WHERE r.primary_guest_id IS NULL AND r.status = 'pending' AND r.check_out_date > $3::date)::int,
		       coalesce((SELECT nullif(settings->>'checkin_time', '') FROM shared.tenants WHERE schema_name = current_schema()), '15:00')
		FROM reservations r
		WHERE ($1::bool OR r.property_id = ANY($2::uuid[]))
		  AND r.check_out_date >= $3::date`, all, ids, today).
		Scan(&out.Arrivals, &out.Departures, &out.InHouse, &out.Incomplete, &out.CheckinTime)
	return out, err
}

// OccupiedToday devuelve las propiedades con huesped en casa hoy (pines del
// mapa del Inicio).
func OccupiedToday(ctx context.Context, q database.Querier, today string) (map[uuid.UUID]bool, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT property_id FROM reservations
		WHERE status = 'checked_in' OR (status = 'confirmed' AND check_in_date <= $1::date AND check_out_date > $1::date)`, today)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// Today es el resumen del dia del actor (GET /reservations/today).
func (s *Service) Today(ctx context.Context, actor authz.Actor) (Today, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Today, error) {
		t, err := s.tenant(ctx, q)
		if err != nil {
			return Today{}, err
		}
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return Today{}, err
		}
		return TodaySummary(ctx, q, scope, t.Today)
	})
}
