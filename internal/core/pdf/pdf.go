// Package pdf escribe documentos PDF sencillos sin dependencias: paginas A4
// con texto en Helvetica (normal y negrita, con acentos via WinAnsiEncoding),
// lineas y rectangulos de color. Alcanza para la boleta de pago (fase05.md §3).
//
// Las coordenadas van en puntos desde la esquina superior izquierda (y crece
// hacia abajo), como se piensa una hoja; el paquete las convierte al sistema
// de PDF.
package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Tamaño A4 en puntos.
const (
	A4Width  = 595.28
	A4Height = 841.89
)

// Font es la familia de letra.
type Font uint8

// Fuentes estandar (no se incrustan: todo lector de PDF las trae).
const (
	Regular Font = iota
	Bold
)

// Color en RGB de 0 a 255.
type Color struct{ R, G, B uint8 }

// Hex crea un color desde "#rrggbb".
func Hex(s string) Color {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return Color{}
	}
	var c [3]uint8
	for i := range c {
		v, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return Color{}
		}
		c[i] = uint8(v)
	}
	return Color{c[0], c[1], c[2]}
}

func (c Color) ops() string {
	return fmt.Sprintf("%s %s %s", num(float64(c.R)/255), num(float64(c.G)/255), num(float64(c.B)/255))
}

// Doc es un documento en construccion.
type Doc struct {
	title string
	pages []*Page
}

// Page es una pagina A4.
type Page struct {
	ops bytes.Buffer
}

// New crea un documento con su titulo (metadatos).
func New(title string) *Doc {
	return &Doc{title: title}
}

// AddPage agrega una pagina A4 y la devuelve.
func (d *Doc) AddPage() *Page {
	p := &Page{}
	d.pages = append(d.pages, p)
	return p
}

// Text escribe s con la linea base en (x, y).
func (p *Page) Text(x, y, size float64, f Font, c Color, s string) {
	fmt.Fprintf(&p.ops, "BT /F%d %s Tf %s rg %s %s Td (%s) Tj ET\n",
		f+1, num(size), c.ops(), num(x), num(A4Height-y), literal(s))
}

// TextRight escribe s alineado a la derecha en x.
func (p *Page) TextRight(x, y, size float64, f Font, c Color, s string) {
	p.Text(x-Width(s, size, f), y, size, f, c, s)
}

// Rect dibuja un rectangulo relleno con esquina superior izquierda en (x, y).
func (p *Page) Rect(x, y, w, h float64, fill Color) {
	fmt.Fprintf(&p.ops, "%s rg %s %s %s %s re f\n", fill.ops(), num(x), num(A4Height-y-h), num(w), num(h))
}

// Line dibuja una linea recta.
func (p *Page) Line(x1, y1, x2, y2, width float64, c Color) {
	fmt.Fprintf(&p.ops, "%s RG %s w %s %s m %s %s l S\n", c.ops(), num(width), num(x1), num(A4Height-y1), num(x2), num(A4Height-y2))
}

// Fit recorta s con "…" para que no pase de max puntos.
func Fit(s string, size float64, f Font, maxWidth float64) string {
	if Width(s, size, f) <= maxWidth {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && Width(string(r)+"…", size, f) > maxWidth {
		r = r[:len(r)-1]
	}
	return strings.TrimSpace(string(r)) + "…"
}

// Bytes devuelve el PDF.
func (d *Doc) Bytes() ([]byte, error) {
	if len(d.pages) == 0 {
		d.AddPage()
	}
	var out bytes.Buffer
	offsets := []int{0}
	obj := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets)-1, body)
	}
	out.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	// 1 catalogo, 2 arbol de paginas, 3 y 4 fuentes, 5 info; luego pagina + contenido.
	kids := make([]string, len(d.pages))
	for i := range d.pages {
		kids[i] = fmt.Sprintf("%d 0 R", 6+i*2)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(d.pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	obj(fmt.Sprintf("<< /Title (%s) /Producer (ARGOS) >>", literal(d.title)))
	for i, p := range d.pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			num(A4Width), num(A4Height), 7+i*2))
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		if _, err := zw.Write(p.ops.Bytes()); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		obj(fmt.Sprintf("<< /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", z.Len(), z.String()))
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, o := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R /Info 5 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.Bytes(), nil
}

// literal codifica s para una cadena PDF (WinAnsi) escapando \ ( ).
func literal(s string) string {
	var b strings.Builder
	for _, r := range s {
		c, ok := winAnsi(r)
		if !ok {
			c = '?'
		}
		switch c {
		case '\\', '(', ')':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			if c < 32 {
				continue
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}

// winAnsi convierte una runa al byte de WinAnsiEncoding (cp1252).
func winAnsi(r rune) (byte, bool) {
	switch {
	case r < 128, r >= 0xA0 && r <= 0xFF:
		return byte(r), true //nolint:gosec // r ya esta en 0..255
	}
	if c, ok := cp1252Extra[r]; ok {
		return c, true
	}
	return 0, false
}

var cp1252Extra = map[rune]byte{
	'€': 0x80, '‚': 0x82, '„': 0x84, '…': 0x85, '†': 0x86, '‡': 0x87, '‰': 0x89, '‹': 0x8B,
	'‘': 0x91, '’': 0x92, '“': 0x93, '”': 0x94, '•': 0x95, '–': 0x96, '—': 0x97, '™': 0x99, '›': 0x9B,
}

// num escribe un numero con hasta tres decimales.
func num(f float64) string {
	return strconv.FormatFloat(math.Round(f*1000)/1000, 'f', -1, 64)
}
