package app

import "testing"

func TestNormalizeDriveLetter(t *testing.T) {
	valid := []struct{ in, want string }{
		{"", ""},
		{"D", "D"},
		{"d", "D"},
		{"D:", "D"},
		{"d:", "D"},
		{"V", "V"},
		{"Z:", "Z"},
	}
	for _, c := range valid {
		got, err := NormalizeDriveLetter(c.in)
		if err != nil {
			t.Errorf("NormalizeDriveLetter(%q) returned error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeDriveLetter(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	invalid := []string{
		"C",    // システムボリュームは Dev Drive にできない
		"c:",   //
		"A",    // フロッピー用に予約
		"B",    //
		"DE",   // 2 文字
		"D:\\", // パス区切りつき
		"1",    // 数字
		"あ",    // 非 ASCII
		":",    // レターなし
	}
	for _, in := range invalid {
		if got, err := NormalizeDriveLetter(in); err == nil {
			t.Errorf("NormalizeDriveLetter(%q) = %q, want error", in, got)
		}
	}
}
