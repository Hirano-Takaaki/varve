//go:build windows

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// PowerShell 側から結果と失敗を区別して受け取るためのマーカー。スクリプトを try/catch
// で包んで自分で書き出すことで、例外が CLIXML として stderr に流れるのを避ける。
const (
	psResultPrefix = "varve-result:"
	psErrorPrefix  = "varve-error:"
)

func mountVHDX(ctx context.Context, path, requestedLetter string, trust bool) (string, error) {
	// 呼び出し側がフラグ解析時に検証済みだが、多層防御としてここでも通す。
	requestedLetter, err := NormalizeDriveLetter(requestedLetter)
	if err != nil {
		return "", err
	}
	trustScript := ""
	if trust {
		trustScript = `& fsutil.exe devdrv trust "${letter}:" | Out-Null; if ($LASTEXITCODE -ne 0) { throw "fsutil devdrv trust failed" };`
	}
	letterLiteral := strings.ReplaceAll(requestedLetter, "'", "''")
	pathLiteral := strings.ReplaceAll(path, "'", "''")
	body := fmt.Sprintf(`
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'mount requires an elevated terminal'
}
$image = Mount-DiskImage -ImagePath '%s' -StorageType VHDX -PassThru
$partition = $image | Get-Disk | Get-Partition | Where-Object Type -ne Reserved | Select-Object -First 1
if (-not $partition) { throw 'VHDX has no usable partition' }
$letter = $partition.DriveLetter
$requested = '%s'
if ($requested -and $letter -ne $requested) {
  if ($letter) { $partition | Set-Partition -NewDriveLetter $requested } else { $partition | Add-PartitionAccessPath -AccessPath "${requested}:" }
  $letter = $requested
} elseif (-not $letter) {
  $used = (Get-Volume | Where-Object DriveLetter).DriveLetter
  $letter = [char[]]('D'..'Z') | Where-Object { $used -notcontains $_ } | Select-Object -First 1
  if (-not $letter) { throw 'no free drive letter' }
  $partition | Add-PartitionAccessPath -AccessPath "${letter}:"
}
%s
Write-Output "%s${letter}:"
`, pathLiteral, letterLiteral, trustScript, psResultPrefix)

	letter, err := runPowerShell(ctx, body)
	if err != nil {
		return "", fmt.Errorf("mount VHDX: %w", err)
	}
	if letter == "" {
		return "", errors.New("mount succeeded without a drive letter")
	}
	return letter, nil
}

// detachVHDX はアタッチ中の VHDX をフラッシュしてからデタッチする。
// アタッチされていなければ何もしない。戻り値はアタッチされていたかと、
// 割り当てられていたドライブレター（再アタッチ時の復元用）。
//
// Dismount-DiskImage はボリュームキャッシュの書き戻しを保証しないため、
// 必ず Write-VolumeCache を前置する。省略すると書き込み直後のデータを失う
// （README「データを失うもの」参照）。
func detachVHDX(ctx context.Context, path string) (bool, string, error) {
	pathLiteral := strings.ReplaceAll(path, "'", "''")
	body := fmt.Sprintf(`
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = [Security.Principal.WindowsPrincipal]::new($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'detach requires an elevated terminal'
}
$image = $null
try { $image = Get-DiskImage -ImagePath '%s' -ErrorAction Stop } catch { $image = $null }
if (-not $image -or -not $image.Attached) {
  Write-Output "%sdetached"
} else {
  $letter = ''
  $volume = Get-Disk -Number $image.Number | Get-Partition | Where-Object DriveLetter |
    Get-Volume | Select-Object -First 1
  if ($volume) {
    $letter = $volume.DriveLetter
    Write-VolumeCache -DriveLetter $volume.DriveLetter -ErrorAction Continue
  }
  $null = Dismount-DiskImage -ImagePath '%s'
  Write-Output "%sattached:${letter}"
}
`, pathLiteral, psResultPrefix, pathLiteral, psResultPrefix)
	result, err := runPowerShell(ctx, body)
	if err != nil {
		return false, "", fmt.Errorf("detach VHDX: %w", err)
	}
	if letter, ok := strings.CutPrefix(result, "attached:"); ok {
		return true, letter, nil
	}
	return false, "", nil
}

func preflightVHDX(ctx context.Context, path string) error {
	pathLiteral := strings.ReplaceAll(path, "'", "''")
	body := fmt.Sprintf(`
$image = $null
try { $image = Get-DiskImage -ImagePath '%s' -ErrorAction Stop } catch { $image = $null }
if ($image -and $image.Attached) { throw 'VHDX is attached; flush and dismount it before snapshot operations' }
Write-Output "%s"
`, pathLiteral, psResultPrefix)
	if _, err := runPowerShell(ctx, body); err != nil {
		return fmt.Errorf("VHDX preflight: %w", err)
	}
	return nil
}

// runPowerShell は body を try/catch で包んで実行し、結果マーカーに続く値を返す。
//
// 例外は PowerShell 自身に stderr へ書かせず、catch した Exception.Message を stdout に
// 平文で出させる。こうしないと CLIXML でエンコードされたうえ、コンソールのコード
// ページで書き出されて日本語が文字化けする。PowerShell の起動自体が失敗した場合は
// マーカーが出ないので、stderr を decodeCLIXML に通してから返す。
func runPowerShell(ctx context.Context, body string) (string, error) {
	script := `$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
try {
` + body + `
} catch {
  Write-Output "` + psErrorPrefix + `$($_.Exception.Message -replace '\r?\n', ' ')"
  exit 1
}
`
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePowerShell(script))
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if message, ok := strings.CutPrefix(line, psErrorPrefix); ok {
			return "", errors.New(message)
		}
		if value, ok := strings.CutPrefix(line, psResultPrefix); ok && runErr == nil {
			return value, nil
		}
	}
	if message := decodeCLIXML(stderr.String()); message != "" {
		if runErr == nil {
			return "", errors.New(message)
		}
		return "", fmt.Errorf("%w: %s", runErr, message)
	}
	if runErr != nil {
		return "", runErr
	}
	return "", errors.New("PowerShell produced no result")
}

func encodePowerShell(s string) string {
	u16 := utf16.Encode([]rune(s))
	b := make([]byte, len(u16)*2)
	for i, v := range u16 {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
