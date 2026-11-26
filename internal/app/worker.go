package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"

	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/modules/attendance"
	"github.com/smorareqs/argos-service/internal/modules/billing"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/metrics"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/securityops"
	"github.com/smorareqs/argos-service/internal/modules/tasks"
)

// NewMail construye el servicio de correo con el driver configurado.
func NewMail(ctx context.Context, cfg config.Config, log *slog.Logger) (*mail.Service, error) {
	var sender mail.Sender
	switch cfg.Mail() {
	case config.MailSES:
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
		if err != nil {
			return nil, fmt.Errorf("configuracion de AWS para SES: %w", err)
		}
		sender = mail.SESSender{Client: sesv2.NewFromConfig(awsCfg)}
	default:
		sender = mail.LogSender{Log: log.With("module", "mail"), Dir: cfg.MailDir}
	}
	return mail.NewService(sender, cfg.SESFrom)
}

// NewJobRegistry registra los handlers de trabajos de cada modulo
// (append-only, reparto.md §2).
func NewJobRegistry(svc Services, mailer *mail.Service) *jobs.Registry {
	r := jobs.NewRegistry()
	r.Register(jobs.TypeSendEmail, func(ctx context.Context, job jobs.Running) error {
		var e jobs.Email
		if err := json.Unmarshal(job.Payload, &e); err != nil {
			return fmt.Errorf("payload de correo invalido: %w", err)
		}
		return mailer.Send(ctx, e.To, e.Template, e.Data)
	}, jobs.ScrubPayload())
	r.Register(jobs.TypeStorageCleanup, func(ctx context.Context, _ jobs.Running) error {
		return svc.Files.Cleanup(ctx)
	})
	r.Every(jobs.TypeStorageCleanup, time.Hour)
	// Fase 3 (Persona A): preventivos y stock bajo, cada hora por empresa.
	r.Register(maintenance.JobMaintenanceDue, svc.Maintenance.HandleJob)
	r.Every(maintenance.JobMaintenanceDue, time.Hour)
	r.Register(maintenance.JobLowStock, svc.Maintenance.HandleJob)
	r.Every(maintenance.JobLowStock, time.Hour)
	// Fase 4 (Persona A): iCal cada 30 min, entradas y salidas automaticas cada
	// hora, recordatorio al huesped (programado) y retencion de documentos.
	r.Register(reservations.JobIcalSync, svc.Reservations.HandleJob)
	r.Every(reservations.JobIcalSync, 30*time.Minute)
	r.Register(reservations.JobStatus, svc.Reservations.HandleJob)
	r.Every(reservations.JobStatus, time.Hour)
	r.Register(reservations.JobReminder, svc.Reservations.HandleJob)
	r.Register(jobs.TypeRetentionCleanup, func(ctx context.Context, _ jobs.Running) error {
		_, err := svc.Files.Retention(ctx)
		return err
	})
	r.Every(jobs.TypeRetentionCleanup, time.Hour)
	// Fase 6 (Persona A): facturacion diaria (factura, vence, suspende).
	r.Register(billing.JobCycle, svc.Billing.HandleJob)
	r.Every(billing.JobCycle, 24*time.Hour)
	// Fase 2 (Persona B): tareas retrasadas cada 5 minutos por empresa.
	r.Register(tasks.JobMarkLate, svc.Tasks.HandleJob)
	r.Every(tasks.JobMarkLate, 5*time.Minute)
	// Fase 3 (Persona B): push de cada aviso y recordatorio 30 min antes.
	r.Register(notifications.JobSendPush, svc.Notifications.HandleJob)
	r.Register(notifications.JobTaskReminders, svc.Notifications.HandleJob)
	r.Every(notifications.JobTaskReminders, 5*time.Minute)
	// Fase 4 (Persona B): push al huesped y alarmas de salida cada minuto.
	r.Register(notifications.JobSendGuestPush, svc.Notifications.HandleJob, jobs.ScrubPayload())
	r.Register(securityops.JobAlarms, svc.Security.HandleJob)
	r.Every(securityops.JobAlarms, time.Minute)
	// Fase 5 (Persona B): marcajes sin salida cada 15 min; foto del desempeno
	// e incentivos de las metas una vez al dia.
	r.Register(attendance.JobAutoclose, svc.Attendance.HandleJob)
	r.Every(attendance.JobAutoclose, 15*time.Minute)
	r.Register(metrics.JobSnapshot, svc.Metrics.HandleJob)
	r.Every(metrics.JobSnapshot, 24*time.Hour)
	r.Register(metrics.JobEvaluate, svc.Metrics.HandleJob)
	r.Every(metrics.JobEvaluate, 24*time.Hour)
	return r
}

// NewWorker arma el worker con todos los handlers.
func NewWorker(ctx context.Context, cfg config.Config, db *database.DB, log *slog.Logger, svc Services) (*jobs.Worker, error) {
	mailer, err := NewMail(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	return jobs.NewWorker(db, NewJobRegistry(svc, mailer), log.With("module", "worker"), cfg.WorkerConcurrency), nil
}
