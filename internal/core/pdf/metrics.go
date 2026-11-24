package pdf

// Anchos de Helvetica y Helvetica-Bold (AFM estandar de Adobe, milesimas de
// em) para los caracteres 32 a 126. Las letras acentuadas usan el ancho de su
// letra base. Sirven para alinear a la derecha y recortar textos.

var regularWidths = [95]int{
	278, 278, 355, 556, 556, 889, 667, 191, 333, 333, 389, 584, 278, 333, 278, 278, // espacio a /
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556, // 0-9
	278, 278, 584, 584, 584, 556, 1015, // : ; < = > ? @
	667, 667, 722, 722, 667, 611, 778, 722, 278, 500, 667, 556, 833, // A-M
	722, 778, 667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611, // N-Z
	278, 278, 278, 469, 556, 333, // [ \ ] ^ _ `
	556, 556, 500, 556, 556, 278, 556, 556, 222, 222, 500, 222, 833, // a-m
	556, 556, 556, 556, 333, 500, 278, 556, 500, 722, 500, 500, 500, // n-z
	334, 260, 334, 584, // { | } ~
}

var boldWidths = [95]int{
	278, 333, 474, 556, 556, 889, 722, 238, 333, 333, 389, 584, 278, 333, 278, 278,
	556, 556, 556, 556, 556, 556, 556, 556, 556, 556,
	333, 333, 584, 584, 584, 611, 975,
	722, 722, 722, 722, 667, 611, 778, 722, 278, 556, 722, 611, 833,
	722, 778, 667, 778, 722, 667, 611, 722, 667, 944, 667, 667, 611,
	333, 278, 333, 584, 556, 333,
	556, 611, 556, 611, 556, 333, 611, 611, 278, 278, 556, 278, 889,
	611, 611, 611, 611, 389, 556, 333, 611, 556, 778, 556, 556, 500,
	389, 280, 389, 584,
}

// base es la letra sin acento que da el ancho de los caracteres latinos.
var base = map[rune]rune{
	'á': 'a', 'à': 'a', 'ä': 'a', 'â': 'a', 'é': 'e', 'è': 'e', 'ë': 'e', 'ê': 'e',
	'í': 'i', 'ì': 'i', 'ï': 'i', 'î': 'i', 'ó': 'o', 'ò': 'o', 'ö': 'o', 'ô': 'o',
	'ú': 'u', 'ù': 'u', 'ü': 'u', 'û': 'u', 'ñ': 'n', 'ç': 'c',
	'Á': 'A', 'À': 'A', 'Ä': 'A', 'É': 'E', 'È': 'E', 'Í': 'I', 'Ó': 'O', 'Ö': 'O',
	'Ú': 'U', 'Ü': 'U', 'Ñ': 'N', 'Ç': 'C',
}

// Width es el ancho en puntos de s a ese tamaño.
func Width(s string, size float64, f Font) float64 {
	table := &regularWidths
	if f == Bold {
		table = &boldWidths
	}
	total := 0
	for _, r := range s {
		total += runeWidth(r, table)
	}
	return float64(total) * size / 1000
}

func runeWidth(r rune, table *[95]int) int {
	if b, ok := base[r]; ok {
		r = b
	}
	if r >= 32 && r <= 126 {
		return table[r-32]
	}
	switch r {
	case '…', '—', '‰':
		return 1000
	case '·', '¡':
		return 278
	}
	return 556
}
