// Package xlsx escribe libros de Excel (Office Open XML) sencillos sin
// dependencias: hojas con encabezado fijo, anchos de columna y celdas de texto,
// numero, dinero o fecha. Alcanza para exportar la nomina (fase05.md §3).
package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind es el formato de una columna.
type Kind uint8

// Formatos de columna.
const (
	Text   Kind = iota // texto
	Number             // numero con dos decimales
	Money              // numero con separador de miles y dos decimales
	Int                // entero
	Date               // fecha (time.Time o "AAAA-MM-DD")
)

// Column describe una columna de la hoja.
type Column struct {
	Header string
	Width  float64 // en caracteres; 0 = 14
	Kind   Kind
}

// Sheet es una hoja: columnas, filas y, opcional, una fila de totales en negrita.
type Sheet struct {
	Name    string
	Columns []Column
	Rows    [][]any
	Totals  []any
}

// Workbook es el libro completo.
type Workbook struct {
	Sheets []Sheet
}

// Estilos (indices en cellXfs de styles.xml).
const (
	styleDefault = iota
	styleHeader
	styleNumber
	styleMoney
	styleInt
	styleDate
	styleTotalText
	styleTotalMoney
	styleTotalNumber
)

// Bytes devuelve el archivo .xlsx.
func (w Workbook) Bytes() ([]byte, error) {
	if len(w.Sheets) == 0 {
		return nil, fmt.Errorf("xlsx: el libro no tiene hojas")
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	files := map[string]string{
		"[Content_Types].xml":        contentTypes(len(w.Sheets)),
		"_rels/.rels":                rootRels,
		"xl/workbook.xml":            workbook(w.Sheets),
		"xl/_rels/workbook.xml.rels": workbookRels(len(w.Sheets)),
		"xl/styles.xml":              styles,
	}
	order := []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/styles.xml"}
	for i, s := range w.Sheets {
		name := fmt.Sprintf("xl/worksheets/sheet%d.xml", i+1)
		files[name] = sheetXML(s)
		order = append(order, name)
	}
	for _, name := range order {
		f, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte(files[name])); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func contentTypes(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

const rootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
	`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
	`</Relationships>`

func workbook(sheets []Sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for i, s := range sheets {
		fmt.Fprintf(&b, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, esc(sheetName(s.Name, i)), i+1, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	return b.String()
}

func workbookRels(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, i, i)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, n+1)
	b.WriteString(`</Relationships>`)
	return b.String()
}

// styles: fuente normal y negrita, relleno del encabezado y formatos numericos
// (164 = #,##0.00; 165 = AAAA-MM-DD).
const styles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
	`<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
	`<numFmts count="2"><numFmt numFmtId="164" formatCode="#,##0.00"/><numFmt numFmtId="165" formatCode="yyyy-mm-dd"/></numFmts>` +
	`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
	`<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill>` +
	`<fill><patternFill patternType="solid"><fgColor rgb="FFE6EEEC"/><bgColor indexed="64"/></patternFill></fill></fills>` +
	`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
	`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
	`<cellXfs count="9">` +
	`<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>` +
	`<xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/>` +
	`<xf numFmtId="2" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="1" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="165" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>` +
	`<xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/>` +
	`<xf numFmtId="164" fontId="1" fillId="2" borderId="0" xfId="0" applyNumberFormat="1" applyFont="1" applyFill="1"/>` +
	`<xf numFmtId="2" fontId="1" fillId="2" borderId="0" xfId="0" applyNumberFormat="1" applyFont="1" applyFill="1"/>` +
	`</cellXfs></styleSheet>`

func sheetXML(s Sheet) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>` +
		`<cols>`)
	for i, c := range s.Columns {
		w := c.Width
		if w <= 0 {
			w = 14
		}
		fmt.Fprintf(&b, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, i+1, i+1, strconv.FormatFloat(w, 'f', 1, 64))
	}
	b.WriteString(`</cols><sheetData>`)
	header := make([]any, len(s.Columns))
	for i, c := range s.Columns {
		header[i] = c.Header
	}
	writeRow(&b, 1, header, s.Columns, true, false)
	for i, r := range s.Rows {
		writeRow(&b, i+2, r, s.Columns, false, false)
	}
	if s.Totals != nil {
		writeRow(&b, len(s.Rows)+2, s.Totals, s.Columns, false, true)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.String()
}

func writeRow(b *strings.Builder, n int, cells []any, cols []Column, header, total bool) {
	fmt.Fprintf(b, `<row r="%d">`, n)
	for i, v := range cells {
		if i >= len(cols) || v == nil {
			continue
		}
		ref := colName(i) + strconv.Itoa(n)
		kind := cols[i].Kind
		if header {
			kind = Text
		}
		writeCell(b, ref, v, kind, cellStyle(kind, header, total))
	}
	b.WriteString(`</row>`)
}

func cellStyle(k Kind, header, total bool) int {
	switch {
	case header:
		return styleHeader
	case total && (k == Money):
		return styleTotalMoney
	case total && (k == Number || k == Int):
		return styleTotalNumber
	case total:
		return styleTotalText
	}
	switch k {
	case Number:
		return styleNumber
	case Money:
		return styleMoney
	case Int:
		return styleInt
	case Date:
		return styleDate
	}
	return styleDefault
}

func writeCell(b *strings.Builder, ref string, v any, k Kind, style int) {
	if num, ok := number(v); ok && k != Text {
		fmt.Fprintf(b, `<c r="%s" s="%d"><v>%s</v></c>`, ref, style, strconv.FormatFloat(num, 'f', -1, 64))
		return
	}
	if k == Date {
		if serial, ok := dateSerial(v); ok {
			fmt.Fprintf(b, `<c r="%s" s="%d"><v>%d</v></c>`, ref, style, serial)
			return
		}
	}
	fmt.Fprintf(b, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, esc(fmt.Sprint(v)))
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// dateSerial convierte una fecha al numero de serie de Excel (dias desde 1899-12-30).
func dateSerial(v any) (int, bool) {
	var t time.Time
	switch d := v.(type) {
	case time.Time:
		t = d
	case string:
		p, err := time.Parse(time.DateOnly, d)
		if err != nil {
			return 0, false
		}
		t = p
	default:
		return 0, false
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return int(day.Sub(base).Hours() / 24), true
}

// colName convierte 0 → A, 25 → Z, 26 → AA.
func colName(i int) string {
	name := ""
	for i >= 0 {
		name = string(rune('A'+i%26)) + name
		i = i/26 - 1
	}
	return name
}

// sheetName limpia el nombre: Excel no acepta []:*?/\ ni mas de 31 caracteres.
func sheetName(name string, i int) string {
	clean := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	if clean == "" {
		clean = fmt.Sprintf("Hoja %d", i+1)
	}
	if r := []rune(clean); len(r) > 31 {
		clean = string(r[:31])
	}
	return clean
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
