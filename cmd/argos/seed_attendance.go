package main

import (
	"context"
	"fmt"
	"time"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/metrics"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 5 de Persona B: marcajes de los dos ultimos meses segun
// el horario de cada colaborador (con algun retraso y alguna ausencia), metas
// con sus incentivos pendientes y la foto diaria del desempeno. Corre antes de
// la nomina de demostracion (Persona A), que calcula con estos marcajes.

var demoGoals = []metrics.GoalWrite{
	{Name: "Limpiezas del mes", Metric: "cleanings_done", Target: 30, PeriodKind: "monthly", IncentiveAmount: ptr(250.0)},
	{Name: "Sin retrasos en el mes", Metric: "late_count", Target: 1, PeriodKind: "monthly",
		AppliesTo: "role", AppliesRole: ptr(string(authz.RoleCleaner)), IncentiveAmount: ptr(100.0)},
	{Name: "Checklist completo en la semana", Metric: "checklist_compliance", Target: 95, PeriodKind: "weekly",
		RewardText: ptr("Reconocimiento en el tablero")},
}

func seedAttendance(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	for slug, t := range ids {
		ops, ok := demoOperations[slug]
		if !ok {
			continue
		}
		admin, err := services.Auth.UserByEmail(ctx, ops.admin)
		if err != nil {
			return err
		}
		tctx := database.WithTenantSchema(ctx, t.SchemaName)
		actor := authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()}
		if err := seedAttendanceTenant(tctx, services, actor, t); err != nil {
			return fmt.Errorf("asistencia y metas de %s: %w", slug, err)
		}
	}
	fmt.Println("Asistencia, metas e incentivos de demostración listos.")
	return nil
}

func seedAttendanceTenant(ctx context.Context, services app.Services, actor authz.Actor, t tenants.Tenant) error {
	db := services.Attendance.DB()
	var done bool
	if err := db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attendance_records)`).Scan(&done)
	}); err != nil || done {
		return err
	}
	today := time.Now()
	from := quincenas(today)[0][0].AddDate(0, 0, -15)
	// Un marcaje por tramo del horario (sin horario: lunes a viernes de 8 a
	// 16). Entrada y salida con +-14 min, 1 de cada 20 dias ausente.
	if err := db.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `
			WITH tz AS (SELECT coalesce((SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala') AS name),
			days AS (SELECT d::date AS d FROM generate_series($1::date, ($2::date - 1), interval '1 day') d),
			plan AS (
				SELECT e.id AS emp, days.d, s.starts_at, s.ends_at
				FROM employees e CROSS JOIN days
				JOIN employee_schedules s ON s.employee_id = e.id AND s.weekday = extract(dow FROM days.d)
				WHERE e.hire_date <= days.d AND (e.termination_date IS NULL OR e.termination_date >= days.d)
				UNION ALL
				SELECT e.id, days.d, '08:00'::time, '16:00'::time
				FROM employees e CROSS JOIN days
				WHERE NOT EXISTS (SELECT 1 FROM employee_schedules s WHERE s.employee_id = e.id)
				  AND extract(dow FROM days.d) BETWEEN 1 AND 5
				  AND e.hire_date <= days.d AND (e.termination_date IS NULL OR e.termination_date >= days.d)
			),
			punch AS (
				SELECT emp, ((d + starts_at) AT TIME ZONE tz.name) + make_interval(mins => (hashtext(emp::text || d::text) % 15)) AS cin,
				       ((d + ends_at) AT TIME ZONE tz.name) + make_interval(mins => (hashtext(d::text || emp::text) % 15)) AS cout,
				       hashtext(emp::text || 'x' || d::text) AS h
				FROM plan CROSS JOIN tz
			)
			INSERT INTO attendance_records (employee_id, clock_in_at, clock_out_at, in_lat, in_lng, out_lat, out_lng, source, worked_minutes)
			SELECT emp, cin, cout, 14.5986, -90.5094, 14.5987, -90.5093, 'app',
			       floor(extract(epoch FROM cout - cin) / 60)::int
			FROM punch WHERE abs(h) % 20 <> 0 AND cout > cin`, from.Format(time.DateOnly), today.Format(time.DateOnly))
		return err
	}); err != nil {
		return err
	}
	for _, g := range demoGoals {
		if _, err := services.Metrics.CreateGoal(ctx, actor, g); err != nil {
			return err
		}
	}
	return db.InTx(ctx, func(q database.Querier) error {
		for d := today.AddDate(0, 0, -30); d.Before(today); d = d.AddDate(0, 0, 1) {
			day, _ := time.Parse(time.DateOnly, d.Format(time.DateOnly))
			if _, err := services.Metrics.Snapshot(ctx, q, day); err != nil {
				return err
			}
		}
		_, err := services.Metrics.Evaluate(ctx, q, t.ID)
		return err
	})
}
