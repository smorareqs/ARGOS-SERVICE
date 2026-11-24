package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/disciplinary"
	"github.com/smorareqs/argos-service/internal/modules/payroll"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Datos de RRHH de demostracion (fase 5, parte de Persona A): en Garrent,
// tres quincenas cerradas con historial (para que Bono 14 y aguinaldo tengan
// de donde promediar), la quincena en curso calculada en borrador y tres
// acciones disciplinarias (una con acuse). Las limpiezas por colaboradora se
// corrigen en revision porque el itinerario (Persona B) aun no existe.

// demoCleanings son las limpiezas validadas por quincena cerrada (por correo).
var demoCleanings = map[string][]int{"maria.ramos@garrent.gt": {19, 22, 18}}

// demoOvertime son horas extra registradas a mano por quincena cerrada.
var demoOvertime = map[string][]float64{"luis.torres@garrent.gt": {0, 390, 0}, "sofia.mendez@garrent.gt": {162.5, 0, 0}}

func seedHR(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	t, ok := ids["garrent"]
	if !ok {
		return nil
	}
	admin, err := services.Auth.UserByEmail(ctx, demoOperations["garrent"].admin)
	if err != nil {
		return err
	}
	ctx = database.WithTenantSchema(ctx, t.SchemaName)
	actor := authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()}
	if err := seedPayroll(ctx, services, actor); err != nil {
		return fmt.Errorf("nómina de demostración: %w", err)
	}
	if err := seedDiscipline(ctx, services, actor); err != nil {
		return fmt.Errorf("disciplina de demostración: %w", err)
	}
	fmt.Println("Nómina y disciplina de demostración listas.")
	return nil
}

// quincenas devuelve las tres quincenas cerradas anteriores a la actual y la actual.
func quincenas(today time.Time) [][2]time.Time {
	start := func(t time.Time) time.Time {
		if t.Day() <= 15 {
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		return time.Date(t.Year(), t.Month(), 16, 0, 0, 0, 0, time.UTC)
	}
	end := func(s time.Time) time.Time {
		if s.Day() == 1 {
			return s.AddDate(0, 0, 14)
		}
		return time.Date(s.Year(), s.Month()+1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	}
	current := start(today)
	out := [][2]time.Time{{current, end(current)}}
	for range 3 {
		prev := start(out[0][0].AddDate(0, 0, -1))
		out = append([][2]time.Time{{prev, end(prev)}}, out...)
	}
	return out
}

func seedPayroll(ctx context.Context, services app.Services, actor authz.Actor) error {
	existing, err := services.Payroll.Periods(ctx, actor)
	if err != nil || len(existing) > 0 {
		return err
	}
	users := map[uuid.UUID]string{}
	for email := range mergeKeys(demoCleanings, demoOvertime) {
		u, err := services.Auth.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		users[u.ID] = email
	}
	list := quincenas(time.Now())
	for i, q := range list {
		// La quincena en curso queda calculada en borrador; las anteriores, cerradas.
		if err := seedPeriod(ctx, services, actor, q, users, i, i < len(list)-1); err != nil {
			return err
		}
	}
	return nil
}

func seedPeriod(ctx context.Context, services app.Services, actor authz.Actor, q [2]time.Time, users map[uuid.UUID]string, i int, closeIt bool) error {
	p, err := services.Payroll.CreatePeriod(ctx, actor, payroll.PeriodCreate{
		Kind: payroll.KindBiweekly, PeriodStart: q[0].Format(time.DateOnly), PeriodEnd: q[1].Format(time.DateOnly),
	})
	if err != nil {
		return err
	}
	d, err := services.Payroll.Compute(ctx, actor, p.ID)
	if err != nil || !closeIt {
		return err
	}
	if _, err := services.Payroll.Review(ctx, actor, p.ID); err != nil {
		return err
	}
	for _, e := range d.Entries {
		if err := demoCorrections(ctx, services, actor, p.ID, e, users[e.UserID], i); err != nil {
			return err
		}
	}
	_, err = services.Payroll.Close(ctx, actor, p.ID)
	return err
}

func demoCorrections(ctx context.Context, services app.Services, actor authz.Actor, periodID uuid.UUID, e payroll.Line, email string, i int) error {
	var in payroll.EntryUpdate
	changed := false
	if c, ok := demoCleanings[email]; ok && i < len(c) {
		in.CleaningsCount, changed = optional.Of(c[i]), true
	}
	if o, ok := demoOvertime[email]; ok && i < len(o) && o[i] > 0 {
		in.OvertimeAmount, changed = optional.Of(o[i]), true
	}
	if !changed {
		return nil
	}
	reason := "Conteo del supervisor mientras el itinerario no está en ARGOS."
	in.Reason = &reason
	_, err := services.Payroll.UpdateEntry(ctx, actor, periodID, e.ID, in)
	return err
}

func mergeKeys[A, B any](a map[string]A, b map[string]B) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

type demoAction struct {
	email, kind, description string
	daysAgo                  int
	suspension               int // dias de suspension
	ack                      string
}

var demoActions = []demoAction{
	{
		"luis.torres@garrent.gt", "verbal", "Llegó 40 minutos tarde a la revisión del aire acondicionado de TB-504 sin avisar al supervisor.", 26, 0,
		"Se me ponchó la llanta camino a la zona 10. Ya guardé el número del supervisor para avisar.",
	},
	{"maria.ramos@garrent.gt", "written", "Dejó la llave de Casa San Sebastián en la caja de seguridad equivocada después de la limpieza del 3 de noviembre.", 12, 0, ""},
	{"diego.castillo@garrent.gt", "suspension", "Segunda vez que deja una incidencia marcada como resuelta sin haber cambiado la pieza (A/C de Cayalá 8C).", 4, 2, ""},
}

func seedDiscipline(ctx context.Context, services app.Services, actor authz.Actor) error {
	existing, _, err := services.Disciplinary.List(ctx, actor, disciplinary.Filter{Limit: 1})
	if err != nil || len(existing) > 0 {
		return err
	}
	staff, err := services.Employees.List(ctx, nil)
	if err != nil {
		return err
	}
	for _, a := range demoActions {
		user, err := services.Auth.UserByEmail(ctx, a.email)
		if err != nil {
			return err
		}
		employeeID := uuid.Nil
		for _, e := range staff {
			if e.UserID == user.ID {
				employeeID = e.ID
			}
		}
		if employeeID == uuid.Nil {
			continue
		}
		if err := seedAction(ctx, services, actor, a, employeeID, user.ID); err != nil {
			return err
		}
	}
	return nil
}

func seedAction(ctx context.Context, services app.Services, actor authz.Actor, a demoAction, employeeID, userID uuid.UUID) error {
	in := disciplinary.ActionCreate{
		EmployeeID: employeeID, Type: a.kind, Description: a.description,
		OccurredOn: time.Now().AddDate(0, 0, -a.daysAgo).Format(time.DateOnly),
	}
	if a.suspension > 0 {
		from := time.Now().AddDate(0, 0, 3).Format(time.DateOnly)
		to := time.Now().AddDate(0, 0, 2+a.suspension).Format(time.DateOnly)
		in.SuspensionFrom, in.SuspensionTo = &from, &to
	}
	created, err := services.Disciplinary.Create(ctx, actor, in)
	if err != nil || a.ack == "" {
		return err
	}
	note := a.ack
	own := authz.Actor{UserID: userID, TenantID: actor.TenantID}
	_, err = services.Disciplinary.Acknowledge(ctx, own, created.ID, disciplinary.Acknowledge{Note: &note})
	return err
}
