package mbim

import (
	"context"
	"errors"
	"testing"
)

func TestEncodeUICCOpenChannel(t *testing.T) {
	aid := []byte{0xA0, 0x00, 0x00, 0x05, 0x59}
	info := encodeUICCOpenChannel(aid, 0, 0)
	if le.Uint32(info[0:]) != uint32(len(aid)) {
		t.Fatalf("AppId size = %d, want %d", le.Uint32(info[0:]), len(aid))
	}
	if le.Uint32(info[4:]) != 16 {
		t.Fatalf("AppId offset = %d, want 16", le.Uint32(info[4:]))
	}
	if string(info[16:16+len(aid)]) != string(aid) {
		t.Fatal("AID not written")
	}
}

// libmbim 的 uicc-ref-byte-array 编码(_mbim_struct_builder_append_byte_array,
// swapped_offset_length=true)对空数组有特殊处理:buffer_len==0 时 offset 字段写 0,
// 不指向变长区。旧实现无论 AID 是否为空都写 offset=fixed(16),空 AID 时这个非零
// offset 指向一段空数据——不符合协议、可能是真机上空 AID 开通道返回
// status=0x15(InvalidParameters)的原因。
func TestEncodeUICCOpenChannelEmptyAIDWritesZeroOffset(t *testing.T) {
	info := encodeUICCOpenChannel(nil, 0, 0)
	if le.Uint32(info[0:]) != 0 {
		t.Fatalf("AppId size = %d, want 0", le.Uint32(info[0:]))
	}
	if le.Uint32(info[4:]) != 0 {
		t.Fatalf("AppId offset = %d, want 0 (libmbim writes 0 offset for empty uicc-ref-byte-array)", le.Uint32(info[4:]))
	}
}

func TestEncodeUICCAPDU(t *testing.T) {
	cmd := []byte{0x00, 0xA4, 0x04, 0x00, 0x02, 0x3F, 0x00}
	info := encodeUICCAPDU(1, cmd)
	if le.Uint32(info[0:]) != 1 {
		t.Fatalf("Channel = %d", le.Uint32(info[0:]))
	}
	if le.Uint32(info[12:]) != uint32(len(cmd)) || le.Uint32(info[16:]) != 20 {
		t.Fatalf("Command size/offset = %d/%d", le.Uint32(info[12:]), le.Uint32(info[16:]))
	}
	if string(info[20:20+len(cmd)]) != string(cmd) {
		t.Fatal("APDU not written")
	}
}

// 新固件 OPEN_CHANNEL 的 status 字段是 SELECT 的 SW1:0x90 表示成功、通道已打开。
// 旧实现把"非 0 即失败"判错,会拒掉这类设备。
func TestUICCOpenChannelAcceptsSW1Success(t *testing.T) {
	for _, status := range []uint32{0x00, 0x90, 0x91, 0x61, 0x9000} {
		st := status
		ft := newFakeTransport()
		ft.reply = func(w []byte) ([]byte, bool) {
			h, _ := decodeHeader(w)
			switch h.Type {
			case MessageTypeOpen:
				return openDoneMsg(h.TransactionID), true
			case MessageTypeCommand:
				resp := make([]byte, 16)
				le.PutUint32(resp[0:], st) // status = SW1(或完整 SW)
				le.PutUint32(resp[4:], 1)  // channel = 1
				return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCOpenChannel, resp), true
			}
			return nil, false
		}
		d := newDevice(ft)
		if err := d.Open(context.Background(), 4096); err != nil {
			t.Fatalf("Open: %v", err)
		}
		ch, err := UICCOpenChannel(context.Background(), d, []byte{0xA0})
		d.Close()
		if err != nil {
			t.Fatalf("status=0x%x: UICCOpenChannel 报错: %v", st, err)
		}
		if ch != 1 {
			t.Fatalf("status=0x%x: channel = %d, want 1", st, ch)
		}
	}
}

// 真正的错误 SW1(如 0x6A 文件未找到)仍应失败。
func TestUICCOpenChannelRejectsErrorSW1(t *testing.T) {
	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			resp := make([]byte, 16)
			le.PutUint32(resp[0:], 0x6A) // SW1=0x6A 错误
			le.PutUint32(resp[4:], 0)
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCOpenChannel, resp), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := UICCOpenChannel(context.Background(), d, []byte{0xA0}); err == nil {
		t.Fatal("status=0x6A 应返回错误")
	}
}

func TestUICCAPDUStatusErrorWrapsKnownMSStatus(t *testing.T) {
	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			msg := makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCAPDU, nil)
			le.PutUint32(msg[40:], StatusMSInvalidLogicalChannel)
			return msg, true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	_, err := UICCAPDU(context.Background(), d, 1, []byte{0x00, 0xA4})
	if err == nil {
		t.Fatal("UICCAPDU: want error, got nil")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("UICCAPDU error = %v (%T), want *StatusError", err, err)
	}
	if se.Status != StatusMSInvalidLogicalChannel {
		t.Fatalf("StatusError.Status = 0x%x, want 0x%x", se.Status, StatusMSInvalidLogicalChannel)
	}
}

// MBIM 把卡的 SW 放在 Status 字段、Response 只含数据。LPA 需要完整 R-APDU(数据+SW),
// 故 UICCAPDU 必须把 Status 里的 SW 追加回数据尾部。否则 LPA 会把数据末两字节误当 SW。
func TestUICCAPDUAppendsStatusWordFromStatusField(t *testing.T) {
	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			data := []byte{0xBF, 0x3E, 0x12, 0x5A, 0x10} // 仅数据,无 SW
			resp := make([]byte, 12+len(data))
			le.PutUint32(resp[0:], 0x90)              // Status = SW1 0x90(此固件成功)
			le.PutUint32(resp[4:], uint32(len(data))) // Response length
			le.PutUint32(resp[8:], 12)                // Response offset
			copy(resp[12:], data)
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCAPDU, resp), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	out, err := UICCAPDU(context.Background(), d, 1, []byte{0xBF, 0x3E, 0x03, 0x5C, 0x01, 0x5A})
	if err != nil {
		t.Fatalf("UICCAPDU: %v", err)
	}
	want := []byte{0xBF, 0x3E, 0x12, 0x5A, 0x10, 0x90, 0x00} // 数据 + SW(9000)
	if len(out) != len(want) {
		t.Fatalf("response len = %d, want %d (%x)", len(out), len(want), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("response = %x, want %x", out, want)
		}
	}
}

func TestUICCStatusWordBytes(t *testing.T) {
	cases := []struct {
		status uint32
		want   [2]byte
	}{
		{0x90, [2]byte{0x90, 0x00}},   // 仅 SW1
		{0x9000, [2]byte{0x90, 0x00}}, // 完整 SW
		{0x61, [2]byte{0x61, 0x00}},   // 仅 SW1(更多数据)
		{0x6310, [2]byte{0x63, 0x10}}, // 完整 SW(告警)
	}
	for _, c := range cases {
		got := uiccStatusWordBytes(c.status)
		if got[0] != c.want[0] || got[1] != c.want[1] {
			t.Fatalf("uiccStatusWordBytes(0x%x) = %x, want %x", c.status, got, c.want)
		}
	}
}

func TestUICCAPDURoundTrip(t *testing.T) {
	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			resp := make([]byte, 12+2)
			le.PutUint32(resp[0:], 0)
			le.PutUint32(resp[4:], 2)
			le.PutUint32(resp[8:], 12)
			resp[12], resp[13] = 0x90, 0x00
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCAPDU, resp), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	out, err := UICCAPDU(context.Background(), d, 1, []byte{0x00, 0xA4})
	if err != nil {
		t.Fatalf("UICCAPDU: %v", err)
	}
	if len(out) != 2 || out[0] != 0x90 || out[1] != 0x00 {
		t.Fatalf("APDU response = %x", out)
	}
}
