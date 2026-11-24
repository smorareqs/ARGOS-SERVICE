package payroll

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/pdf"
	"github.com/smorareqs/argos-service/internal/core/xlsx"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Tipos de contenido de los documentos.
const (
	ContentTypeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	ContentTypePDF  = "application/pdf"
)

// employeeExtra son datos de la ficha laboral que van en el Excel y la boleta.
type employeeExtra struct {
	Document, IGSS, NIT *string
	Hire                time.Time
}

func extras(ctx context.Context, q database.Querier, list []Line) (map[uuid.UUID]employeeExtra, error) {
	ids := make([]uuid.UUID, len(list))
	for i, e := range list {
		ids[i] = e.EmployeeID
	}
	rows, err := q.Query(ctx, `SELECT id, document_number, igss_number, nit, hire_date FROM employees WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]employeeExtra{}
	for rows.Next() {
		var id uuid.UUID
		var x employeeExtra
		if err := rows.Scan(&id, &x.Document, &x.IGSS, &x.NIT, &x.Hire); err != nil {
			return nil, err
		}
		out[id] = x
	}
	return out, rows.Err()
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// fileSlug arma un nombre de archivo sin acentos ni espacios.
func fileSlug(s string) string {
	r := strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ñ", "n", "Á", "A", "É", "E", "Í", "I", "Ó", "O", "Ú", "U", "Ñ", "N", "–", "-", " ", "-")
	var b strings.Builder
	for _, c := range r.Replace(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return strings.ToLower(strings.Trim(b.String(), "-"))
}

// --- Excel -------------------------------------------------------------------

// ExportXLSX genera el Excel del periodo (hojas Nomina, Bono 14 o aguinaldo, y
// Asistencia).
func (s *Service) ExportXLSX(ctx context.Context, _ authz.Actor, id uuid.UUID) (File, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (File, error) {
		d, err := s.detail(ctx, q, id)
		if err != nil {
			return File{}, err
		}
		body, err := s.workbook(ctx, q, d)
		if err != nil {
			return File{}, err
		}
		return File{Name: "nomina-" + fileSlug(d.Name) + ".xlsx", ContentType: ContentTypeXLSX, Body: body}, nil
	})
}

func (s *Service) workbook(ctx context.Context, q database.Querier, d PeriodDetail) ([]byte, error) {
	cfg, err := settings(ctx, q)
	if err != nil {
		return nil, err
	}
	x, err := extras(ctx, q, d.Entries)
	if err != nil {
		return nil, err
	}
	m := func(h string) xlsx.Column { return xlsx.Column{Header: h, Width: 13, Kind: xlsx.Money} }
	main := xlsx.Sheet{Name: "Nómina", Columns: []xlsx.Column{
		{Header: "Código", Width: 10},
		{Header: "Nombre", Width: 28},
		{Header: "DPI", Width: 16},
		{Header: "IGSS", Width: 13},
		{Header: "NIT", Width: 12},
		{Header: "Puesto", Width: 20},
		{Header: "Esquema", Width: 16},
		m("Tarifa"),
		{Header: "En planilla", Width: 11},
		{Header: "Días", Width: 8, Kind: xlsx.Number},
		{Header: "Horas", Width: 9, Kind: xlsx.Number},
		{Header: "Limpiezas", Width: 10, Kind: xlsx.Int},
		m("Salario base"), m("Horas extra"), m("Incentivos"), m("Otros devengos"),
		m("Bonif. incentivo"), m("Bono 14"), m("Aguinaldo"), m("Total devengado"), m("IGSS laboral"), m("Otras deducciones"),
		m("Líquido"), m("Cuota patronal"),
		{Header: "Notas", Width: 30},
	}}
	for _, e := range d.Entries {
		ex := x[e.EmployeeID]
		patronal := 0.0
		if e.OnPayroll {
			patronal = round2((e.BaseAmount + e.OvertimeAmount + e.OtherEarnings) * cfg.IGSSEmployerRate)
		}
		yes := "No"
		if e.OnPayroll {
			yes = "Sí"
		}
		main.Rows = append(main.Rows, []any{
			e.EmployeeCode, e.FullName, str(ex.Document), str(ex.IGSS), str(ex.NIT), e.Position, PayTypeLabel(e.PayType), e.PayRate,
			yes, e.DaysWorked, e.HoursWorked, e.CleaningsCount, e.BaseAmount, e.OvertimeAmount, e.IncentivesAmount, e.OtherEarnings,
			e.IncentiveBonusAmount, e.Bonus14Amount, e.AguinaldoAmount, e.GrossAmount, e.IGSSEmployee, e.OtherDeductions,
			e.NetAmount, patronal, str(e.Notes),
		})
	}
	t := d.Totals
	main.Totals = []any{
		"Total", fmt.Sprintf("%d colaboradores", len(d.Entries)), nil, nil, nil, nil, nil, nil, nil, t.Days, t.Hours,
		t.Cleanings, t.Base, t.Overtime, t.Incentives, t.OtherEarnings, t.IncentiveBonus, t.Bonus14, t.Aguinaldo, t.Gross, t.IGSS,
		t.OtherDeductions, t.Net, t.IGSSEmployer,
	}
	sheets := []xlsx.Sheet{main}
	if bonus := bonusSheet(d.Entries); bonus != nil {
		sheets = append(sheets, *bonus)
	}
	att := xlsx.Sheet{Name: "Asistencia", Columns: []xlsx.Column{
		{Header: "Código", Width: 10},
		{Header: "Nombre", Width: 28},
		{Header: "Origen", Width: 18},
		{Header: "Días", Width: 8, Kind: xlsx.Number},
		{Header: "Horas", Width: 9, Kind: xlsx.Number},
		{Header: "Limpiezas", Width: 10, Kind: xlsx.Int},
	}}
	for _, e := range d.Entries {
		src := "Marcajes"
		if e.AttendanceSource == "schedule" {
			src = "Horario semanal"
		}
		att.Rows = append(att.Rows, []any{e.EmployeeCode, e.FullName, src, e.DaysWorked, e.HoursWorked, e.CleaningsCount})
	}
	sheets = append(sheets, att)
	return xlsx.Workbook{Sheets: sheets}.Bytes()
}

func bonusSheet(list []Line) *xlsx.Sheet {
	sh := xlsx.Sheet{Columns: []xlsx.Column{
		{Header: "Código", Width: 10},
		{Header: "Nombre", Width: 28},
		{Header: "Concepto", Width: 12},
		{Header: "Desde", Width: 12, Kind: xlsx.Date},
		{Header: "Hasta", Width: 12, Kind: xlsx.Date},
		{Header: "Meses con nómina", Width: 16, Kind: xlsx.Int},
		{Header: "Meses estimados", Width: 15, Kind: xlsx.Int},
		{Header: "Devengado", Width: 13, Kind: xlsx.Money},
		{Header: "Promedio (÷ 12)", Width: 15, Kind: xlsx.Money},
	}}
	kinds := map[string]bool{}
	for _, e := range list {
		for _, b := range e.BonusDetail {
			kinds[b.Kind] = true
			sh.Rows = append(sh.Rows, []any{
				e.EmployeeCode, e.FullName, bonusLabel(b.Kind), b.WindowStart, b.WindowEnd,
				b.MonthsHistory, b.MonthsEstimated, b.Earnings, b.Amount,
			})
		}
	}
	if len(sh.Rows) == 0 {
		return nil
	}
	switch {
	case kinds["bonus14"] && kinds["aguinaldo"]:
		sh.Name = "Bono 14 y aguinaldo"
	case kinds["bonus14"]:
		sh.Name = "Bono 14"
	default:
		sh.Name = "Aguinaldo"
	}
	return &sh
}

func bonusLabel(kind string) string {
	if kind == "bonus14" {
		return "Bono 14"
	}
	return "Aguinaldo"
}

// --- Boletas -----------------------------------------------------------------

// Payslip genera la boleta de una linea. Quien no tiene hr.payroll.export solo
// obtiene la suya y solo de periodos cerrados (404 en las demas).
func (s *Service) Payslip(ctx context.Context, actor authz.Actor, entryID uuid.UUID) (File, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (File, error) {
		var periodID, userID uuid.UUID
		var status string
		err := q.QueryRow(ctx, `SELECT e.period_id, em.user_id, p.status FROM payroll_entries e
			JOIN employees em ON em.id = e.employee_id JOIN payroll_periods p ON p.id = e.period_id WHERE e.id = $1`, entryID).
			Scan(&periodID, &userID, &status)
		if err != nil {
			return File{}, notFound(err, ErrEntryNotFound)
		}
		manager := actor.Permissions[authz.HRPayrollExport] || actor.Permissions[authz.HRPayrollManage]
		if !manager && (userID != actor.UserID || status != StatusClosed) {
			return File{}, ErrEntryNotFound
		}
		d, err := s.detail(ctx, q, periodID)
		if err != nil {
			return File{}, err
		}
		var entry Line
		for _, e := range d.Entries {
			if e.ID == entryID {
				entry = e
			}
		}
		body, err := s.payslips(ctx, q, d, []Line{entry})
		if err != nil {
			return File{}, err
		}
		return File{Name: "boleta-" + fileSlug(d.Name) + "-" + fileSlug(entry.EmployeeCode) + ".pdf", ContentType: ContentTypePDF, Body: body}, nil
	})
}

// PeriodPayslips genera todas las boletas del periodo en un PDF (una por pagina).
func (s *Service) PeriodPayslips(ctx context.Context, _ authz.Actor, id uuid.UUID) (File, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (File, error) {
		d, err := s.detail(ctx, q, id)
		if err != nil {
			return File{}, err
		}
		if len(d.Entries) == 0 {
			return File{}, ErrNothingToClose
		}
		body, err := s.payslips(ctx, q, d, d.Entries)
		if err != nil {
			return File{}, err
		}
		return File{Name: "boletas-" + fileSlug(d.Name) + ".pdf", ContentType: ContentTypePDF, Body: body}, nil
	})
}

func (s *Service) payslips(ctx context.Context, q database.Querier, d PeriodDetail, list []Line) ([]byte, error) {
	t, err := s.tenant(ctx, q)
	if err != nil {
		return nil, err
	}
	cfg, err := settings(ctx, q)
	if err != nil {
		return nil, err
	}
	x, err := extras(ctx, q, list)
	if err != nil {
		return nil, err
	}
	title := "Boletas · " + d.Name
	if len(list) == 1 {
		title = "Boleta · " + d.Name + " · " + list[0].FullName
	}
	doc := pdf.New(title)
	for _, e := range list {
		drawPayslip(doc.AddPage(), slipData{tenant: t, period: d.PeriodRead, entry: e, extra: x[e.EmployeeID], cfg: cfg, now: s.now()})
	}
	return doc.Bytes()
}

// slipData es lo que lleva una boleta.
type slipData struct {
	tenant tenantInfo
	period PeriodRead
	entry  Line
	extra  employeeExtra
	cfg    Settings
	now    time.Time
}

var (
	inkTeal   = pdf.Hex("#003841")
	inkMuted  = pdf.Hex("#3a6168")
	inkWashed = pdf.Hex("#e6eeec")
	inkBody   = pdf.Hex("#5a5a52")
	inkSoft   = pdf.Hex("#7d7d72")
	inkRule   = pdf.Hex("#e2ded3")
	inkAccent = pdf.Hex("#e0a961")
	inkWhite  = pdf.Color{R: 255, G: 255, B: 255}
)

func drawPayslip(p *pdf.Page, s slipData) {
	e, cash := s.entry, func(v float64) string { return money(s.tenant.Currency, v) }
	start, _ := time.Parse(time.DateOnly, s.period.PeriodStart)
	end, _ := time.Parse(time.DateOnly, s.period.PeriodEnd)

	p.Rect(0, 0, pdf.A4Width, 104, inkTeal)
	p.Text(40, 48, 20, pdf.Bold, inkWhite, pdf.Fit(s.tenant.Name, 20, pdf.Bold, 300))
	p.Text(40, 72, 10, pdf.Regular, inkWashed, "Boleta de pago · "+s.period.Name)
	p.TextRight(555, 48, 10, pdf.Bold, inkAccent, "BOLETA DE PAGO")
	p.TextRight(555, 72, 10, pdf.Regular, inkWashed, "Del "+longDate(start)+" al "+longDate(end))

	p.Text(40, 146, 15, pdf.Bold, inkTeal, pdf.Fit(e.FullName, 15, pdf.Bold, 330))
	p.Text(40, 164, 10, pdf.Regular, inkBody, pdf.Fit(e.EmployeeCode+" · "+e.Position, 10, pdf.Regular, 330))
	ids := []string{}
	if s.extra.Document != nil {
		ids = append(ids, "DPI "+*s.extra.Document)
	}
	if s.extra.IGSS != nil {
		ids = append(ids, "IGSS "+*s.extra.IGSS)
	}
	if s.extra.NIT != nil {
		ids = append(ids, "NIT "+*s.extra.NIT)
	}
	p.TextRight(555, 146, 9, pdf.Regular, inkSoft, strings.Join(ids, " · "))
	p.TextRight(555, 164, 9, pdf.Regular, inkSoft, "Ingreso: "+shortDate(s.extra.Hire))

	facts := []struct{ label, value string }{
		{"ESQUEMA", PayTypeLabel(e.PayType)},
		{"TARIFA", cash(e.PayRate)},
		{"DÍAS", trimNum(e.DaysWorked)},
		{"HORAS", trimNum(e.HoursWorked)},
		{"LIMPIEZAS", fmt.Sprint(e.CleaningsCount)},
		{"PLANILLA", yesNo(e.OnPayroll)},
	}
	p.Rect(40, 184, 515, 54, inkWashed)
	for i, f := range facts {
		x := 54 + float64(i)*84
		p.Text(x, 204, 7.5, pdf.Bold, inkMuted, f.label)
		p.Text(x, 224, 11, pdf.Bold, inkTeal, pdf.Fit(f.value, 11, pdf.Bold, 78))
	}

	earn := []row{
		{"Salario base", e.BaseAmount, true},
		{"Horas extra", e.OvertimeAmount, false},
		{"Incentivos", e.IncentivesAmount, false},
		{"Otros devengos", e.OtherEarnings, false},
		{"Bonificación incentivo", e.IncentiveBonusAmount, false},
		{"Bono 14", e.Bonus14Amount, false},
		{"Aguinaldo", e.AguinaldoAmount, false},
	}
	deduct := []row{
		{fmt.Sprintf("IGSS laboral (%s %%)", trimNum(s.cfg.IGSSEmployeeRate*100)), e.IGSSEmployee, e.OnPayroll},
		{"Otras deducciones", e.OtherDeductions, false},
	}
	y1 := column(p, 40, 290, 272, "DEVENGOS", earn, "Total devengado", e.GrossAmount, cash)
	y2 := column(p, 315, 555, 272, "DEDUCCIONES", deduct, "Total deducciones", round2(e.IGSSEmployee+e.OtherDeductions), cash)
	y := max(y1, y2) + 24

	p.Rect(40, y, 515, 58, inkTeal)
	p.Text(58, y+25, 8, pdf.Bold, inkAccent, "LÍQUIDO A RECIBIR")
	p.Text(58, y+43, 9, pdf.Regular, inkWashed, "Devengado menos deducciones")
	p.TextRight(537, y+38, 22, pdf.Bold, inkWhite, cash(e.NetAmount))
	y += 84

	for _, b := range e.BonusDetail {
		ws, _ := time.Parse(time.DateOnly, b.WindowStart)
		we, _ := time.Parse(time.DateOnly, b.WindowEnd)
		line := fmt.Sprintf("%s: %s devengado del %s al %s ÷ 12 = %s (%d meses con nómina en ARGOS, %d estimados con el salario actual).",
			bonusLabel(b.Kind), cash(b.Earnings), shortDate(ws), shortDate(we), cash(b.Amount), b.MonthsHistory, b.MonthsEstimated)
		y = paragraph(p, 40, y, 515, 8.5, inkSoft, line) + 6
	}
	if e.Notes != nil {
		paragraph(p, 40, y+4, 515, 9, inkBody, "Nota: "+*e.Notes)
	}

	p.Line(40, 742, 250, 742, 0.6, inkRule)
	p.Line(345, 742, 555, 742, 0.6, inkRule)
	p.Text(40, 756, 8.5, pdf.Regular, inkSoft, "Firma del colaborador")
	p.Text(345, 756, 8.5, pdf.Regular, inkSoft, "Recursos humanos · "+pdf.Fit(s.tenant.Name, 8.5, pdf.Regular, 120))
	p.Line(40, 790, 555, 790, 0.4, inkRule)
	p.Text(40, 806, 7.5, pdf.Regular, inkSoft, "Generada con ARGOS el "+longDate(s.now)+". Conserva esta boleta como comprobante de pago.")
}

type row struct {
	label  string
	amount float64
	always bool
}

// column dibuja una lista de conceptos con su total y devuelve donde termina.
func column(p *pdf.Page, x, right, y float64, title string, rows []row, totalLabel string, total float64, format func(float64) string) float64 {
	p.Text(x, y, 8, pdf.Bold, inkMuted, title)
	y += 10
	p.Line(x, y, right, y, 0.6, inkRule)
	y += 18
	for _, r := range rows {
		if r.amount == 0 && !r.always {
			continue
		}
		p.Text(x, y, 10, pdf.Regular, inkBody, r.label)
		p.TextRight(right, y, 10, pdf.Regular, inkTeal, format(r.amount))
		y += 19
	}
	y += 2
	p.Line(x, y-12, right, y-12, 0.6, inkRule)
	p.Text(x, y+4, 10, pdf.Bold, inkTeal, totalLabel)
	p.TextRight(right, y+4, 10, pdf.Bold, inkTeal, format(total))
	return y + 4
}

// paragraph escribe texto con saltos de linea al ancho indicado.
func paragraph(p *pdf.Page, x, y, width, size float64, c pdf.Color, text string) float64 {
	line := ""
	for _, w := range strings.Fields(text) {
		next := strings.TrimSpace(line + " " + w)
		if pdf.Width(next, size, pdf.Regular) > width && line != "" {
			p.Text(x, y, size, pdf.Regular, c, line)
			y += size * 1.45
			next = w
		}
		line = next
	}
	if line != "" {
		p.Text(x, y, size, pdf.Regular, c, line)
	}
	return y + size*1.45
}

func trimNum(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func yesNo(b bool) string {
	if b {
		return "Sí"
	}
	return "No"
}
