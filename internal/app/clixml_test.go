package app

import "testing"

func TestDecodeCLIXML(t *testing.T) {
	// 非管理者で --mount したときに実際に観測された出力。
	elevated := `#< CLIXML
<Objs Version="1.1.0.1" xmlns="http://schemas.microsoft.com/powershell/2004/04">` +
		`<S S="Error">mount requires an elevated terminal_x000D__x000A_</S>` +
		`<S S="Error">発生場所 行:6 文字:3_x000D__x000A_</S>` +
		`<S S="Error">+   throw 'mount requires an elevated terminal'_x000D__x000A_</S>` +
		`<S S="Error">    + CategoryInfo          : OperationStopped: (mount requires an elevated terminal:String) [], RuntimeException_x000D__x000A_</S>` +
		`<S S="Error">    + FullyQualifiedErrorId : mount requires an elevated terminal_x000D__x000A_</S>` +
		`<S S="Error"> _x000D__x000A_</S></Objs>`

	if got, want := decodeCLIXML(elevated), "mount requires an elevated terminal"; got != want {
		t.Errorf("decodeCLIXML(elevated) = %q, want %q", got, want)
	}

	cases := []struct{ name, in, want string }{
		{
			name: "先頭要素が空白なら次の要素を採用する",
			in: `#< CLIXML
<Objs Version="1.1.0.1"><S S="Error"> _x000D__x000A_</S><S S="Error">VHDX has no usable partition_x000D__x000A_</S></Objs>`,
			want: "VHDX has no usable partition",
		},
		{
			name: "Error 以外のストリームは無視する",
			in: `#< CLIXML
<Objs Version="1.1.0.1"><S S="Verbose">noise_x000D__x000A_</S><S S="Error">real failure_x000D__x000A_</S></Objs>`,
			want: "real failure",
		},
		{
			name: "XML エンティティを復元する",
			in: `#< CLIXML
<Objs Version="1.1.0.1"><S S="Error">drive &lt;V&gt; &amp; more_x000D__x000A_</S></Objs>`,
			want: "drive <V> & more",
		},
		{
			name: "CLIXML でない入力はそのまま返す",
			in:   "plain error text",
			want: "plain error text",
		},
		{
			name: "空入力は空を返す",
			in:   "   \n  ",
			want: "",
		},
		{
			name: "壊れた XML は情報を落とさずそのまま返す",
			in:   "#< CLIXML\n<Objs><S S=\"Error\">truncated",
			want: "#< CLIXML\n<Objs><S S=\"Error\">truncated",
		},
		{
			name: "Error ストリームが無ければそのまま返す",
			in:   `#< CLIXML` + "\n" + `<Objs Version="1.1.0.1"><S S="Debug">only debug</S></Objs>`,
			want: `#< CLIXML` + "\n" + `<Objs Version="1.1.0.1"><S S="Debug">only debug</S></Objs>`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeCLIXML(c.in); got != c.want {
				t.Errorf("decodeCLIXML() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestUnescapeCLIXML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"line_x000D__x000A_", "line\r\n"},
		{"tab_x0009_end", "tab\tend"},
		{"under_x005F_score", "under_score"},
		{"日本語_x000A_", "日本語\n"},
		{"no escapes", "no escapes"},
		{"_xZZZZ_", "_xZZZZ_"}, // 16 進として読めない指定は触らない
		{"_x00_", "_x00_"},     // 桁数が足りない指定は触らない
	}
	for _, c := range cases {
		if got := unescapeCLIXML(c.in); got != c.want {
			t.Errorf("unescapeCLIXML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
