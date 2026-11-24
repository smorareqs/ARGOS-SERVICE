package pdf

import (
	"bytes"
	"compress/zlib"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDocument(t *testing.T) {
	d := New("Boleta · Quincena 1–15 nov")
	p := d.AddPage()
	p.Rect(40, 40, 515, 60, Hex("#003841"))
	p.Text(56, 76, 16, Bold, Color{255, 255, 255}, "Garrent — Boleta de pago")
	p.Text(56, 130, 10, Regular, Hex("#5a5a52"), "María Ramos (E-001) · Señora de la limpieza (sic)")
	p.TextRight(555, 130, 10, Bold, Hex("#003841"), "Q 2,452.08")
	p.Line(40, 140, 555, 140, 0.5, Hex("#e2ded3"))
	data, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "%PDF-1.4") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatal("encabezado o cierre inválidos")
	}
	// La tabla xref apunta a cada objeto.
	m := regexp.MustCompile(`startxref\n(\d+)\n`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("sin startxref")
	}
	at, _ := strconv.Atoi(m[1])
	if !strings.HasPrefix(s[at:], "xref\n0 8\n") {
		t.Fatalf("startxref no apunta a la tabla: %q", s[at:at+12])
	}
	for i, off := range regexp.MustCompile(`(\d{10}) 00000 n`).FindAllStringSubmatch(s, -1) {
		o, _ := strconv.Atoi(off[1])
		if want := strconv.Itoa(i+1) + " 0 obj"; !strings.HasPrefix(s[o:], want) {
			t.Errorf("xref %d apunta a %q", i+1, s[o:o+10])
		}
	}
	// El contenido descomprimido trae el texto en WinAnsi con los paréntesis escapados.
	start := strings.Index(s, "stream\n") + len("stream\n")
	end := strings.Index(s, "\nendstream")
	zr, err := zlib.NewReader(bytes.NewReader(data[start:end]))
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(zr)
	for _, want := range []string{"Garrent \x97 Boleta de pago", "Mar\xeda Ramos \\(E-001\\) \xb7 Se\xf1ora de la limpieza \\(sic\\)", "/F2 10 Tf"} {
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("el contenido no tiene %q", want)
		}
	}
	if !strings.Contains(s, "/Title (Boleta \xb7 Quincena 1\x9615 nov)") {
		t.Error("título sin codificar")
	}
}

func TestWidthAndFit(t *testing.T) {
	// "Q 2,452.08" en Helvetica-Bold 10: Q 778 + espacio 278 + 6 dígitos 556 + coma 278 + punto 278.
	if got, want := Width("Q 2,452.08", 10, Bold), (778+278+556*6+278+278)/100.0; got != want {
		t.Errorf("Width = %v, quería %v", got, want)
	}
	if Width("María", 10, Regular) != Width("Maria", 10, Regular) {
		t.Error("las letras acentuadas miden como su base")
	}
	long := "Torre Botánica 1102, Zona 10, Ciudad de Guatemala"
	fit := Fit(long, 10, Regular, 120)
	if !strings.HasSuffix(fit, "…") || Width(fit, 10, Regular) > 120 {
		t.Errorf("Fit = %q (%v pt)", fit, Width(fit, 10, Regular))
	}
	if Fit("Corto", 10, Regular, 120) != "Corto" {
		t.Error("un texto que cabe no se recorta")
	}
}
