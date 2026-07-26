package app

import (
	"errors"
	"strings"
)

// NormalizeDriveLetter は --drive に渡された値を検証し、大文字 1 文字へ正規化する。
// 空文字は「指定なし」を意味するのでそのまま返す。
//
// マウント直前まで検証を遅らせると、16GB の VHDX を復元し終えてから不正な値で
// 失敗することになる。呼び出し側はフラグ解析の直後にこれを通すこと。
func NormalizeDriveLetter(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	letter := strings.TrimSuffix(strings.ToUpper(value), ":")
	if len(letter) != 1 || letter[0] < 'D' || letter[0] > 'Z' {
		return "", errors.New("drive letter must be D through Z")
	}
	return letter, nil
}
