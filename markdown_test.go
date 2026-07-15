package main

import "testing"

func TestWhatsappToMarkdown(t *testing.T) {
	cases := [][2]string{
		{"file_name_v2.pdf", "file_name_v2.pdf"},
		{"precio: 5*3 unidades\ny 4*2 cajas", "precio: 5*3 unidades\ny 4*2 cajas"},
		{"2 * 3 = 6 y 4 * 2 = 8", "2 * 3 = 6 y 4 * 2 = 8"},
		{"snake_case y otro_ejemplo_aca", "snake_case y otro_ejemplo_aca"},
		{"https://ejemplo.com/foo_bar_baz?x_y=1", "https://ejemplo.com/foo_bar_baz?x_y=1"},
		{"*bold real* y _italica real_", "**bold real** y _italica real_"},
		{"*a* *b*", "**a** **b**"},
		{"(*parens*) y fin *linea*", "(**parens**) y fin **linea**"},
		{"~tachado~ ok", "~~tachado~~ ok"},
		{"aprox ~30 personas y ~50 sillas", "aprox ~30 personas y ~50 sillas"},
		{"*multi\nlinea* no", "*multi\nlinea* no"},
		{"código `no *tocar*` y ```\n*esto* menos\n```", "código `no *tocar*` y ```\n*esto* menos\n```"},
		{"*x*", "**x**"},
		{"intra*word*no", "intra*word*no"},
	}
	for _, c := range cases {
		if got := whatsappToMarkdown(c[0]); got != c[1] {
			t.Errorf("whatsappToMarkdown(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestMarkdownToWhatsApp(t *testing.T) {
	cases := [][2]string{
		{"**bold** y *italic*", "*bold* y _italic_"},
		{"2 ** 3 = 6", "2 ** 3 = 6"},
		{"linea con 2*3 y 4*5", "linea con 2*3 y 4*5"},
		{"a_b_c intraword", "a_b_c intraword"},
		{"# Titulo\ntexto", "*Titulo*\ntexto"},
		{"~~fuera~~", "~fuera~"},
		{"__tambien bold__", "*tambien bold*"},
		{"`code *safe*`", "`code *safe*`"},
	}
	for _, c := range cases {
		if got := markdownToWhatsApp(c[0]); got != c[1] {
			t.Errorf("markdownToWhatsApp(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestRoundTrip(t *testing.T) {
	for _, wa := range []string{"*bold* y _it_ y ~st~", "texto plano 2*3", "*a* *b*"} {
		if rt := markdownToWhatsApp(whatsappToMarkdown(wa)); rt != wa {
			t.Errorf("roundtrip(%q) = %q", wa, rt)
		}
	}
}
