package xlsx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestWorkbook(t *testing.T) {
	w := Workbook{Sheets: []Sheet{
		{
			Name: "Nómina",
			Columns: []Column{
				{Header: "Código", Width: 10},
				{Header: "Nombre", Width: 28},
				{Header: "Días", Kind: Number},
				{Header: "Neto", Kind: Money},
				{Header: "Ingreso", Kind: Date},
				{Header: "Limpiezas", Kind: Int},
			},
			Rows: [][]any{
				{"E-001", "María Ramos <&>", 15.0, 2452.08, "2024-03-01", 22},
				{"E-002", "Luis Torres", 13.5, 3100.0, nil, 0},
			},
			Totals: []any{"Total", nil, 28.5, 5552.08, nil, 22},
		},
		{Name: "Bono 14: julio/junio", Columns: []Column{{Header: "A"}}, Rows: [][]any{{"x"}}},
	}}
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("no es un zip: %v", err)
	}
	parts := map[string]string{}
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		parts[f.Name] = string(b)
		var doc struct{}
		if err := xml.Unmarshal(b, &doc); err != nil && !strings.HasSuffix(f.Name, ".rels") {
			t.Errorf("%s no es XML válido: %v", f.Name, err)
		}
	}
	for _, name := range []string{"[Content_Types].xml", "xl/workbook.xml", "xl/styles.xml", "xl/worksheets/sheet1.xml", "xl/worksheets/sheet2.xml"} {
		if _, ok := parts[name]; !ok {
			t.Errorf("falta %s", name)
		}
	}
	sheet := parts["xl/worksheets/sheet1.xml"]
	for _, want := range []string{
		`<c r="A1" s="1" t="inlineStr"><is><t xml:space="preserve">Código</t>`,
		`María Ramos &lt;&amp;&gt;`,
		`<c r="D2" s="3"><v>2452.08</v>`,
		`<c r="E2" s="5"><v>45352</v>`, // 2024-03-01
		`<c r="F2" s="4"><v>22</v>`,
		`<c r="D4" s="7"><v>5552.08</v>`,
		`state="frozen"`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("la hoja no tiene %q", want)
		}
	}
	if strings.Contains(sheet, `r="E3"`) {
		t.Error("una celda nil no debe escribirse")
	}
	if !strings.Contains(parts["xl/workbook.xml"], `name="Bono 14- julio-junio"`) {
		t.Errorf("nombre de hoja sin limpiar: %s", parts["xl/workbook.xml"])
	}
}

func TestColName(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 51: "AZ", 52: "BA", 701: "ZZ", 702: "AAA"} {
		if got := colName(i); got != want {
			t.Errorf("colName(%d) = %s, quería %s", i, got, want)
		}
	}
}
