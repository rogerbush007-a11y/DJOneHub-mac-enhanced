package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iniwex5/vohive/internal/modem"
)

func TestLegacyADBKey(t *testing.T) {
	// Synthetic challenges, independently checked with openssl passwd -1.
	for challenge, want := range map[string]string{
		"00000000": "C3SV/fMl8Tu3SeK",
		"12345678": "0jXKXQwSwMxYoeg",
		"87654321": "FRtkAuWIXtIsNpt",
		"99999999": "2ajrfuUYlCCWbY3",
	} {
		got, err := legacyADBKey(challenge)
		if err != nil || got != want {
			t.Fatalf("key for synthetic challenge %s = %q, %v; want %q", challenge, got, err, want)
		}
	}
	for _, challenge := range []string{"", "1234567", "123456789", "abcdefgh", "1234567\n", "１２３４５６７８", `1234"678`} {
		if key, err := legacyADBKey(challenge); err == nil || key != "" {
			t.Fatalf("accepted unsupported challenge %q", challenge)
		}
	}
}

type setupATStep struct {
	command  string
	response string
	err      error
}

func scriptedSetupAT(t *testing.T, steps []setupATStep) func(string, time.Duration) (string, error) {
	t.Helper()
	next := 0
	t.Cleanup(func() {
		if next != len(steps) {
			t.Errorf("executed %d AT commands, want %d", next, len(steps))
		}
	})
	return func(command string, timeout time.Duration) (string, error) {
		t.Helper()
		if next >= len(steps) {
			t.Fatalf("unexpected AT command %q", command)
		}
		step := steps[next]
		next++
		if command != step.command || timeout <= 0 {
			t.Fatalf("step %d = %q (%v), want %q with timeout", next, command, timeout, step.command)
		}
		return step.response, step.err
	}
}

func TestPrepareModuleCallUSBAuthorizesBeforeWrite(t *testing.T) {
	for _, identity := range [][2]int{{djiUSBVendorID, djiUSBProductID}, {quectelUSBVendorID, quectelUSBProductID}} {
		for _, uac := range []int{0, 1} {
			original := usbComposition{VendorID: identity[0], ProductID: identity[1], Flags: []int{1, 1, 1, 1, 1, 0, uac}}
			t.Run(original.command(), func(t *testing.T) {
				if original.isCallAudioCapable() {
					t.Fatal("ADB-disabled composition must not be ready")
				}
				target := usbComposition{VendorID: identity[0], ProductID: identity[1], Flags: []int{1, 1, 1, 1, 1, 1, 1}}
				readBack := strings.Replace(target.command(), `AT+QCFG="USBCFG",`, `+QCFG: "usbcfg",`, 1) + "\r\nOK"
				run := scriptedSetupAT(t, []setupATStep{
					{"AT+QADBKEY?", "AT+QADBKEY?\r\n+QADBKEY: 12345678\r\nOK", nil},
					{`AT+QADBKEY="0jXKXQwSwMxYoeg"`, "OK", nil},
					{target.command(), "OK", nil},
					{`AT+QCFG="USBCFG"`, readBack, nil},
				})
				if err := prepareModuleCallUSB(original, run); err != nil {
					t.Fatal(err)
				}
				if original.hasADB() || original.Flags[6] != uac {
					t.Fatal("original rollback configuration was mutated")
				}
			})
		}
	}
}

func TestPrepareModuleCallUSBAlreadyEnabled(t *testing.T) {
	original := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 1}}
	if err := prepareModuleCallUSB(original, scriptedSetupAT(t, nil)); err != nil {
		t.Fatal(err)
	}
	// An enabled ADB bit does not need another persistent authorization.
	original.Flags[6] = 0
	if err := prepareModuleCallUSB(original, scriptedSetupAT(t, []setupATStep{
		{`AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,1,1`, "OK", nil},
		{`AT+QCFG="USBCFG"`, "+QCFG: \"usbcfg\",0x2CA3,0x4006,1,1,1,1,1,1,1\r\nOK", nil},
	})); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareModuleCallUSBStopsBeforeReadbackOnWriteFailure(t *testing.T) {
	for _, response := range []string{"", "ERROR", "AT+QCFG=\"USBCFG\"", "+CME ERROR: 3\r\nOK"} {
		t.Run(response, func(t *testing.T) {
			original := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 1, 0}}
			err := prepareModuleCallUSB(original, scriptedSetupAT(t, []setupATStep{
				{`AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,1,1`, response, nil},
			}))
			if err == nil {
				t.Fatal("unconfirmed USB write accepted")
			}
		})
	}
}

func TestPrepareModuleCallUSBRejectsInvalidOriginal(t *testing.T) {
	if err := prepareModuleCallUSB(usbComposition{}, scriptedSetupAT(t, nil)); err == nil {
		t.Fatal("invalid original configuration accepted")
	}
}

func TestModuleSetupCredentialsDoNotUseLoggingSerialManager(t *testing.T) {
	a := &app{modem: &modem.Manager{}}
	for _, command := range []string{"AT+QADBKEY?", `AT+QADBKEY="synthetic-key"`} {
		response, err := a.runModuleSetupAT(command, time.Second)
		if response != "" || err == nil || err.Error() != "自动 ADB 授权仅支持直接 USB AT 连接" {
			t.Fatalf("credential command must be rejected before serial manager: %q, %v", response, err)
		}
	}
}

func TestPrepareModuleCallUSBStopsOnAuthorizationFailure(t *testing.T) {
	const query = "AT+QADBKEY?"
	const write = `AT+QADBKEY="0jXKXQwSwMxYoeg"`
	for name, steps := range map[string][]setupATStep{
		"unsupported":     {{query, "ERROR", nil}},
		"new protocol":    {{query, "+QADBKEY: abcdef0123456789\r\nOK", nil}},
		"missing OK":      {{query, "+QADBKEY: 12345678", nil}},
		"duplicate":       {{query, "+QADBKEY: 12345678\r\n+QADBKEY: 87654321\r\nOK", nil}},
		"query transport": {{query, "", errors.New("12345678")}},
		"rejected":        {{query, "+QADBKEY: 12345678\r\nOK", nil}, {write, write + "\r\nERROR", nil}},
		"echo only":       {{query, "+QADBKEY: 12345678\r\nOK", nil}, {write, write, nil}},
		"write transport": {{query, "+QADBKEY: 12345678\r\nOK", nil}, {write, "", errors.New(write)}},
	} {
		t.Run(name, func(t *testing.T) {
			original := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
			err := prepareModuleCallUSB(original, scriptedSetupAT(t, steps))
			if err == nil {
				t.Fatal("authorization failure accepted")
			}
			for _, secret := range []string{"12345678", "87654321", "0jXKXQwSwMxYoeg", "abcdef0123456789"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error leaks challenge or key")
				}
			}
		})
	}
}

func TestPrepareModuleCallUSBRejectsUnconfirmedComposition(t *testing.T) {
	for name, readBack := range map[string]string{
		"ADB still disabled": "+QCFG: \"usbcfg\",0x2CA3,0x4006,1,1,1,1,1,0,1\r\nOK",
		"identity changed":   "+QCFG: \"usbcfg\",0x2C7C,0x0125,1,1,1,1,1,1,1\r\nOK",
		"malformed":          "OK",
		"incomplete":         "+QCFG: \"usbcfg\",0x2CA3,0x4006,1,1,1,1,1,1,1",
	} {
		t.Run(name, func(t *testing.T) {
			original := usbComposition{VendorID: djiUSBVendorID, ProductID: djiUSBProductID, Flags: []int{1, 1, 1, 1, 1, 0, 1}}
			err := prepareModuleCallUSB(original, scriptedSetupAT(t, []setupATStep{
				{"AT+QADBKEY?", "+QADBKEY: 12345678\r\nOK", nil},
				{`AT+QADBKEY="0jXKXQwSwMxYoeg"`, "OK", nil},
				{`AT+QCFG="USBCFG",0x2CA3,0x4006,1,1,1,1,1,1,1`, "OK", nil},
				{`AT+QCFG="USBCFG"`, readBack, nil},
			}))
			if err == nil {
				t.Fatal("unconfirmed composition accepted")
			}
		})
	}
}
