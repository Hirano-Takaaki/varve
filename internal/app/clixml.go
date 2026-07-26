package app

import (
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

// PowerShell を -EncodedCommand で起動すると、未処理の例外は stderr に CLIXML として
// 書き出される。そのまま error に載せると、実際のメッセージが XML とエスケープに
// 埋もれて読めない。
//
//	#< CLIXML
//	<Objs Version="1.1.0.1" xmlns="..."><S S="Error">mount requires an elevated terminal_x000D__x000A_</S>...
//
// decodeCLIXML はこれを 1 行のメッセージに戻す。CLIXML でない入力や、解釈できない
// 入力は情報を失わないようそのまま返す。
func decodeCLIXML(raw string) string {
	trimmed := strings.TrimSpace(raw)
	body, isCLIXML := strings.CutPrefix(trimmed, "#< CLIXML")
	if !isCLIXML {
		return trimmed
	}
	var objs struct {
		Strings []struct {
			Stream string `xml:"S,attr"`
			Value  string `xml:",chardata"`
		} `xml:"S"`
	}
	if err := xml.Unmarshal([]byte(strings.TrimSpace(body)), &objs); err != nil {
		return trimmed
	}
	// PowerShell のエラーレコードは表示上の 1 行につき 1 要素で、先頭が例外メッセージ、
	// 以降は発生位置や CategoryInfo といった装飾になる。装飾はロケール依存の文字列で
	// しか判別できないため、位置で切り出す。
	for _, s := range objs.Strings {
		if s.Stream != "Error" {
			continue
		}
		if line := strings.TrimSpace(unescapeCLIXML(s.Value)); line != "" {
			return line
		}
	}
	return trimmed
}

var clixmlEscape = regexp.MustCompile(`_x([0-9A-Fa-f]{4})_`)

// unescapeCLIXML は CLIXML の _xNNNN_ 形式のエスケープを文字に戻す。
// 改行が _x000D__x000A_ として現れるのが主な用途。
func unescapeCLIXML(s string) string {
	return clixmlEscape.ReplaceAllStringFunc(s, func(match string) string {
		code, err := strconv.ParseUint(match[2:6], 16, 32)
		if err != nil {
			return match
		}
		return string(rune(code))
	})
}
