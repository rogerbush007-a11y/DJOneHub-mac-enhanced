package main

import (
	"crypto/md5" // Required by the module's legacy QADBKEY protocol, not password storage.
	"errors"
	"strings"
	"time"
)

// legacyADBKey implements the eight-decimal-digit QADBKEY challenge used by
// QDC507 firmware: the first 15 characters of the MD5-crypt checksum of
// SH_adb_quectel with the challenge as salt. Other challenge schemes must not
// be guessed. Neither the challenge nor the resulting key may be logged.
func legacyADBKey(challenge string) (string, error) {
	if len(challenge) != 8 {
		return "", errors.New("不支持的 ADB 授权格式：需要 8 位数字 QADBKEY，请按模块固件文档授权")
	}
	for _, ch := range challenge {
		if ch < '0' || ch > '9' {
			return "", errors.New("不支持的 ADB 授权格式：需要 8 位数字 QADBKEY，请按模块固件文档授权")
		}
	}
	const password = "SH_adb_quectel"
	initial := md5.New()
	initial.Write([]byte(password + "$1$" + challenge))
	alternate := md5.Sum([]byte(password + challenge + password))
	initial.Write(alternate[:len(password)])
	for n := len(password); n > 0; n >>= 1 {
		if n&1 != 0 {
			initial.Write([]byte{0})
		} else {
			initial.Write([]byte{password[0]})
		}
	}
	digest := initial.Sum(nil)
	for i := 0; i < 1000; i++ {
		h := md5.New()
		if i&1 != 0 {
			h.Write([]byte(password))
		} else {
			h.Write(digest)
		}
		if i%3 != 0 {
			h.Write([]byte(challenge))
		}
		if i%7 != 0 {
			h.Write([]byte(password))
		}
		if i&1 != 0 {
			h.Write(digest)
		} else {
			h.Write([]byte(password))
		}
		digest = h.Sum(nil)
	}
	const alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var encoded strings.Builder
	for _, group := range [][3]int{{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5}} {
		value := uint32(digest[group[0]])<<16 | uint32(digest[group[1]])<<8 | uint32(digest[group[2]])
		for j := 0; j < 4; j++ {
			encoded.WriteByte(alphabet[value&63])
			value >>= 6
		}
	}
	// The protocol only uses 15 checksum characters; the final crypt group
	// (digest[11]) is outside that prefix.
	return encoded.String()[:15], nil
}

func authorizeModuleADB(runAT func(string, time.Duration) (string, error)) error {
	response, err := runAT("AT+QADBKEY?", 5*time.Second)
	if err != nil || atResponseIsError(response) || !atProbeSucceeded(response) {
		return errors.New("无法读取 ADB 授权挑战，未修改 USB 配置；请确认固件支持 QADBKEY")
	}
	var challenge string
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "+QADBKEY:") {
			if challenge != "" {
				return errors.New("ADB 授权响应不明确，未修改 USB 配置")
			}
			challenge = strings.TrimSpace(strings.TrimPrefix(line, "+QADBKEY:"))
		}
	}
	key, err := legacyADBKey(challenge)
	if err != nil {
		return err
	}
	response, err = runAT(`AT+QADBKEY="`+key+`"`, 5*time.Second)
	if err != nil || atResponseIsError(response) || !atProbeSucceeded(response) {
		// Modems may echo the key even on failure. Do not return raw responses
		// or transport errors (which can include the command) to status/logs.
		return errors.New("模块 ADB 授权未确认，未修改 USB 配置；请按模块固件文档检查授权")
	}
	return nil
}

// runModuleSetupAT uses the non-logging direct USB AT path for credentials.
// The serial manager logs commands on timeout even with ExecuteATSilent.
func (a *app) runModuleSetupAT(command string, timeout time.Duration) (string, error) {
	if strings.HasPrefix(command, "AT+QADBKEY") && a.modem != nil {
		return "", errors.New("自动 ADB 授权仅支持直接 USB AT 连接")
	}
	return a.runATCommand(command, timeout)
}

// prepareModuleCallUSB runs only after explicit confirmation and backup.
// ADB authorization is persistent; restoring USBCFG does not revoke it.
func prepareModuleCallUSB(original usbComposition, runAT func(string, time.Duration) (string, error)) error {
	if !original.isRecoverable() {
		return errors.New("模块 USB 配置格式不完整，未修改模块")
	}
	if original.isCallAudioCapable() {
		return nil
	}
	if !original.hasADB() {
		if err := authorizeModuleADB(runAT); err != nil {
			return err
		}
	}
	target := usbComposition{VendorID: original.VendorID, ProductID: original.ProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}}
	// Preserve both supported identities, including DJI's working ECM setup.
	// Keep the previous recovery behavior for other tools' unknown identities.
	if !target.isUACTarget() {
		target.VendorID, target.ProductID = quectelUSBVendorID, quectelUSBProductID
	}
	response, err := runAT(target.command(), 8*time.Second)
	if err != nil || atResponseIsError(response) || !atProbeSucceeded(response) {
		return errors.New("模块未确认 USB 通话配置，未重启模块；请检查原始配置备份")
	}
	readBack, err := runAT(`AT+QCFG="USBCFG"`, 5*time.Second)
	actual, parseErr := parseUSBComposition(readBack)
	if err != nil || parseErr != nil || atResponseIsError(readBack) || !atProbeSucceeded(readBack) || actual.command() != target.command() {
		return errors.New("USB 配置回读未确认 ADB 与 UAC 均已开启，未重启模块；请检查原始配置备份")
	}
	return nil
}
