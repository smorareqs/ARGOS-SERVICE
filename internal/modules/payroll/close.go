package payroll

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Close cierra un periodo en revision: queda inmutable, guarda en S3 el Excel
// y la boleta de cada colaborador, marca pagados los incentivos que sumo,
// avisa por correo a cada colaborador y publica payroll.closed.
func (s *Service) Close(ctx context.Context, actor authz.Actor, id uuid.UUID) (PeriodDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PeriodDetail, error) {
		sp, status, err := lockPeriod(ctx, q, id)
		if err != nil {
			return PeriodDetail{}, err
		}
		switch status {
		case StatusClosed:
			return PeriodDetail{}, ErrClosed
		case StatusDraft:
			return PeriodDetail{}, ErrNotReview
		}
		if _, err := q.Exec(ctx, `UPDATE payroll_periods SET status = 'closed', closed_by = $2, closed_at = now(), updated_at = now()
			WHERE id = $1`, id, actor.UserID); err != nil {
			return PeriodDetail{}, err
		}
		d, err := s.detail(ctx, q, id)
		if err != nil {
			return PeriodDetail{}, err
		}
		if len(d.Entries) == 0 {
			return PeriodDetail{}, ErrNothingToClose
		}
		t, err := s.tenant(ctx, q)
		if err != nil {
			return PeriodDetail{}, err
		}
		if err := s.storeDocuments(ctx, q, t, actor, d); err != nil {
			return PeriodDetail{}, err
		}
		if err := markIncentivesPaid(ctx, q, id, sp); err != nil {
			return PeriodDetail{}, err
		}
		if err := s.notifyClosed(ctx, q, t, actor, d); err != nil {
			return PeriodDetail{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.period_closed", Entity: "payroll_period", EntityID: &id,
			Before: map[string]any{"status": StatusReview},
			After:  map[string]any{"status": StatusClosed, "employees": len(d.Entries), "net_total": d.Totals.Net},
		}); err != nil {
			return PeriodDetail{}, err
		}
		return s.detail(ctx, q, id)
	})
}

// storeDocuments guarda el Excel del periodo y una boleta por linea bajo
// tenants/{tenant_id}/payroll/....
func (s *Service) storeDocuments(ctx context.Context, q database.Querier, t tenantInfo, actor authz.Actor, d PeriodDetail) error {
	book, err := s.workbook(ctx, q, d)
	if err != nil {
		return err
	}
	name := "nomina-" + fileSlug(d.Name) + ".xlsx"
	fileID, err := s.store(ctx, q, t.ID, actor.UserID, "export", name, ContentTypeXLSX, book, "payroll_period", d.ID)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE payroll_periods SET export_file_id = $2 WHERE id = $1`, d.ID, fileID); err != nil {
		return err
	}
	for _, e := range d.Entries {
		slip, err := s.payslips(ctx, q, d, []Line{e})
		if err != nil {
			return err
		}
		name := "boleta-" + fileSlug(d.Name) + "-" + fileSlug(e.EmployeeCode) + ".pdf"
		fileID, err := s.store(ctx, q, t.ID, actor.UserID, "payslip", name, ContentTypePDF, slip, "payroll_entry", e.ID)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE payroll_entries SET payslip_file_id = $2 WHERE id = $1`, e.ID, fileID); err != nil {
			return err
		}
	}
	return nil
}

// store sube un documento generado y registra su fila en files (lista para leer).
func (s *Service) store(ctx context.Context, q database.Querier, tenantID, userID uuid.UUID, kind, name, contentType string, body []byte, entity string, entityID uuid.UUID) (uuid.UUID, error) {
	key, err := s.storage.BuildKey(tenantID, "payroll", kind, name)
	if err != nil {
		return uuid.Nil, err
	}
	if err := s.storage.PutObject(ctx, tenantID, key, contentType, body); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `
		INSERT INTO files (s3_key, module, kind, entity_type, entity_id, content_type, size_bytes, uploaded_by_user_id, original_name, status)
		VALUES ($1, 'payroll', $2, $3, $4, $5, $6, $7, $8, 'ready') RETURNING id`,
		key, kind, entity, entityID, contentType, len(body), userID, name).Scan(&id)
	return id, err
}

// markIncentivesPaid deja pagados los incentivos aprobados que sumo el periodo
// (tabla de Persona B; nada si aun no existe).
func markIncentivesPaid(ctx context.Context, q database.Querier, periodID uuid.UUID, sp span) error {
	ok, err := tableExists(ctx, q, "incentive_awards")
	if err != nil || !ok {
		return err
	}
	_, err = q.Exec(ctx, `
		UPDATE incentive_awards ia SET status = 'paid', payroll_entry_id = e.id
		FROM payroll_entries e
		WHERE e.period_id = $1 AND e.employee_id = ia.employee_id AND e.incentives_amount > 0
		  AND ia.status = 'approved' AND ia.period_end BETWEEN $2 AND $3`, periodID, sp.Start, sp.End)
	return err
}

// notifyClosed avisa por correo a cada colaborador con correo y publica el evento.
func (s *Service) notifyClosed(ctx context.Context, q database.Querier, t tenantInfo, actor authz.Actor, d PeriodDetail) error {
	start, _ := time.Parse(time.DateOnly, d.PeriodStart)
	end, _ := time.Parse(time.DateOnly, d.PeriodEnd)
	ev := ClosedEvent{Period: d.PeriodRead}
	for _, e := range d.Entries {
		ev.Entries = append(ev.Entries, ClosedEntry{EntryID: e.ID, EmployeeID: e.EmployeeID, UserID: e.UserID, NetAmount: e.NetAmount})
		var email string
		if err := q.QueryRow(ctx, `SELECT email FROM shared.users WHERE id = $1`, e.UserID).Scan(&email); err != nil || email == "" {
			continue
		}
		if err := jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{To: email, Template: mail.TemplatePayslipAvailable, Data: map[string]any{
			"Name": firstName(e.FullName), "TenantName": t.Name, "Period": d.Name,
			"Range": "del " + longDate(start) + " al " + longDate(end), "Net": money(t.Currency, e.NetAmount),
		}}); err != nil {
			return err
		}
	}
	return s.bus.Publish(ctx, q, events.Event{Type: EventClosed, TenantID: t.ID, ActorID: &actor.UserID, Data: ev})
}

func firstName(full string) string {
	for i, r := range full {
		if r == ' ' {
			return full[:i]
		}
	}
	return full
}
