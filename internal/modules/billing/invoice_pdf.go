package billing

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/pdf"
)

// File es un documento para descargar.
type File struct {
	Name        string
	ContentType string
	Body        []byte
}

var (
	inkTeal   = pdf.Hex("#003841")
	inkWhite  = pdf.Hex("#ffffff")
	inkWashed = pdf.Hex("#e6efef")
	inkAccent = pdf.Hex("#d9a25f")
	inkBody   = pdf.Hex("#2b2b26")
	inkSoft   = pdf.Hex("#6b6b62")
	inkLine   = pdf.Hex("#dcdcd2")
	inkPaid   = pdf.Hex("#2f7d5b")
	inkDue    = pdf.Hex("#b5582a")
)

// InvoicePDF genera la factura en PDF. tenantID limita a las de la empresa
// (uuid.Nil = plataforma, cualquiera). Una ajena responde 404.
func (s *Service) InvoicePDF(ctx context.Context, tenantID, invoiceID uuid.UUID) (File, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (File, error) {
		list, err := invoicesWhere(ctx, q, `i.id = $1 AND ($2::uuid = '00000000-0000-0000-0000-000000000000' OR i.tenant_id = $2)`,
			invoiceID, tenantID)
		if err != nil {
			return File{}, err
		}
		if len(list) == 0 {
			return File{}, ErrInvoiceNotFound
		}
		inv := list[0]
		var tenant, legal, nit string
		if err := q.QueryRow(ctx, `
			SELECT name, coalesce(settings->>'legal_name', ''), coalesce(settings->>'nit', '')
			FROM shared.tenants WHERE id = $1`, inv.TenantID).Scan(&tenant, &legal, &nit); err != nil {
			return File{}, err
		}
		body, err := drawInvoice(inv, tenant, legal, nit)
		if err != nil {
			return File{}, err
		}
		return File{Name: "factura-" + strings.ToLower(inv.Number) + ".pdf", ContentType: "application/pdf", Body: body}, nil
	})
}

func drawInvoice(inv InvoiceRead, tenant, legal, nit string) ([]byte, error) {
	d := pdf.New("Factura " + inv.Number)
	p := d.AddPage()
	cash := func(v float64) string { return money(inv.Currency, v) }

	p.Rect(0, 0, pdf.A4Width, 104, inkTeal)
	p.Text(40, 50, 22, pdf.Bold, inkWhite, "ARGOS")
	p.Text(40, 74, 10, pdf.Regular, inkWashed, "Operación de propiedades · argos.gt")
	p.TextRight(555, 50, 10, pdf.Bold, inkAccent, "FACTURA")
	p.TextRight(555, 74, 11, pdf.Bold, inkWhite, inv.Number)

	p.Text(40, 146, 8, pdf.Bold, inkSoft, "FACTURADO A")
	name := tenant
	if legal != "" {
		name = legal
	}
	p.Text(40, 166, 14, pdf.Bold, inkTeal, pdf.Fit(name, 14, pdf.Bold, 300))
	if nit != "" {
		p.Text(40, 184, 10, pdf.Regular, inkBody, "NIT "+nit)
	}
	p.TextRight(555, 146, 8, pdf.Bold, inkSoft, "EMITIDA")
	p.TextRight(555, 166, 11, pdf.Regular, inkBody, shortDate(inv.CreatedAt))
	p.TextRight(555, 188, 8, pdf.Bold, inkSoft, "VENCE")
	p.TextRight(555, 206, 11, pdf.Regular, inkBody, shortDate(inv.DueAt))

	p.Rect(40, 236, 515, 30, inkWashed)
	p.Text(52, 255, 8, pdf.Bold, inkSoft, "CONCEPTO")
	p.TextRight(543, 255, 8, pdf.Bold, inkSoft, "MONTO")
	p.Text(52, 292, 11, pdf.Regular, inkBody, pdf.Fit(inv.Description, 11, pdf.Regular, 380))
	p.TextRight(543, 292, 11, pdf.Regular, inkBody, cash(inv.Subtotal))
	p.Line(40, 310, 555, 310, 0.6, inkLine)

	rows := []struct{ label, value string }{{"Subtotal", cash(inv.Subtotal)}, {"IVA (12 %)", cash(inv.Tax)}}
	for i, r := range rows {
		y := 336 + float64(i)*22
		p.Text(360, y, 10, pdf.Regular, inkSoft, r.label)
		p.TextRight(543, y, 10, pdf.Regular, inkBody, r.value)
	}
	p.Rect(350, 372, 205, 40, inkTeal)
	p.Text(362, 397, 10, pdf.Bold, inkWashed, "TOTAL")
	p.TextRight(543, 398, 14, pdf.Bold, inkWhite, cash(inv.Total))

	status, color := "PENDIENTE DE PAGO", inkDue
	switch inv.Status {
	case "paid":
		status, color = "PAGADA", inkPaid
		if inv.PaidAt != nil {
			status += " EL " + strings.ToUpper(shortDate(*inv.PaidAt))
		}
	case "void":
		status, color = "ANULADA", inkSoft
	case "uncollectible":
		status, color = "INCOBRABLE", inkSoft
	}
	p.Text(40, 397, 12, pdf.Bold, color, status)

	p.Line(40, 760, 555, 760, 0.6, inkLine)
	p.Text(40, 780, 8, pdf.Regular, inkSoft, "Precios con IVA incluido. Moneda: "+inv.Currency+". Comprobante generado por ARGOS.")
	return d.Bytes()
}
