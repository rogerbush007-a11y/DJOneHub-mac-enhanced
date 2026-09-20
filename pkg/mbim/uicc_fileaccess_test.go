package mbim

import (
	"bytes"
	"context"
	"testing"
)

// buildUICCAppListInfoForTest 按 libmbim MS UICC Application List 的应答布局构造 InfoBuffer:
// [Version][ApplicationCount][ActiveApplicationIndex][ApplicationListSizeBytes]
// [N × (structOffset, structSize)] [struct blobs...]
// 每个 MbimUiccApplication 结构(固定 32 字节):
// [Type][AppId off,size][Name off,size][PinCount][PinRefs off,size][变长数据]
func buildUICCAppListInfoForTest(apps []UICCApplication) []byte {
	const fixed = 32
	n := len(apps)
	ptrStart := 16
	out := make([]byte, ptrStart+n*8)
	le.PutUint32(out[0:], 1)         // Version
	le.PutUint32(out[4:], uint32(n)) // ApplicationCount
	le.PutUint32(out[8:], 0)         // ActiveApplicationIndex
	dataStart := len(out)
	for i, app := range apps {
		structOff := len(out)
		blob := make([]byte, fixed+pad4(len(app.AID)))
		le.PutUint32(blob[0:], app.Type)
		le.PutUint32(blob[4:], fixed)                // AppId offset (rel to struct)
		le.PutUint32(blob[8:], uint32(len(app.AID))) // AppId size
		// Name/PinRefs 留 0
		copy(blob[fixed:], app.AID)
		out = append(out, blob...)
		le.PutUint32(out[ptrStart+i*8:], uint32(structOff))
		le.PutUint32(out[ptrStart+i*8+4:], uint32(len(blob)))
	}
	le.PutUint32(out[12:], uint32(len(out)-dataStart)) // ApplicationListSizeBytes
	return out
}

func TestUICCReadBinaryEncodesQueryAndParsesResponse(t *testing.T) {
	aid := []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}
	filePath := []byte{0x6F, 0x46} // EF_SPN
	efData := []byte{0x01, 'A', 'T', '&', 'T'}

	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			if le.Uint32(w[36:]) != CIDUICCReadBinary {
				t.Fatalf("CID = %d, want ReadBinary %d", le.Uint32(w[36:]), CIDUICCReadBinary)
			}
			// 校验请求里的 AppId(offset,size)与 ReadSize 字段。
			info := w[48:] // 命令 InfoBuffer 从 header(48) 之后开始(单 fragment)
			aidOff := le.Uint32(info[4:])
			aidSize := le.Uint32(info[8:])
			if int(aidSize) != len(aid) {
				t.Fatalf("AppId size = %d, want %d", aidSize, len(aid))
			}
			if !bytes.Equal(info[aidOff:aidOff+aidSize], aid) {
				t.Fatalf("AppId bytes = %x, want %x", info[aidOff:aidOff+aidSize], aid)
			}
			// 应答:Version, SW1=0x90, SW2=0x00, Data。
			resp := make([]byte, 20+len(efData))
			le.PutUint32(resp[0:], 1)
			le.PutUint32(resp[4:], 0x90)
			le.PutUint32(resp[8:], 0x00)
			le.PutUint32(resp[12:], 20) // Data offset
			le.PutUint32(resp[16:], uint32(len(efData)))
			copy(resp[20:], efData)
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCReadBinary, resp), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	res, err := UICCReadBinary(context.Background(), d, aid, filePath, 0, 0)
	if err != nil {
		t.Fatalf("UICCReadBinary: %v", err)
	}
	if res.SW1 != 0x90 || res.SW2 != 0x00 {
		t.Fatalf("SW = %02x%02x, want 9000", res.SW1, res.SW2)
	}
	if !bytes.Equal(res.Data, efData) {
		t.Fatalf("Data = %x, want %x", res.Data, efData)
	}
}

func TestUICCReadRecordEncodesRecordNumberAndParses(t *testing.T) {
	aid := []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02}
	filePath := []byte{0x6F, 0x40} // EF_MSISDN
	rec := []byte{0xFF, 0xFF, 0x06, 0x91}

	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			if le.Uint32(w[36:]) != CIDUICCReadRecord {
				t.Fatalf("CID = %d, want ReadRecord %d", le.Uint32(w[36:]), CIDUICCReadRecord)
			}
			info := w[48:]
			if got := le.Uint32(info[20:]); got != 2 {
				t.Fatalf("RecordNumber = %d, want 2", got)
			}
			resp := make([]byte, 20+len(rec))
			le.PutUint32(resp[0:], 1)
			le.PutUint32(resp[4:], 0x90)
			le.PutUint32(resp[8:], 0x00)
			le.PutUint32(resp[12:], 20)
			le.PutUint32(resp[16:], uint32(len(rec)))
			copy(resp[20:], rec)
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCReadRecord, resp), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	res, err := UICCReadRecord(context.Background(), d, aid, filePath, 2)
	if err != nil {
		t.Fatalf("UICCReadRecord: %v", err)
	}
	if res.SW1 != 0x90 || !bytes.Equal(res.Data, rec) {
		t.Fatalf("result = %+v, want SW1 0x90 data %x", res, rec)
	}
}

func TestQueryUICCApplicationListParsesAIDs(t *testing.T) {
	usim := []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x02, 0xFF, 0xFF, 0xFF, 0xFF, 0x89, 0x07, 0x09, 0x00, 0x00}
	isim := []byte{0xA0, 0x00, 0x00, 0x00, 0x87, 0x10, 0x04, 0xFF, 0xFF, 0xFF, 0xFF, 0x89, 0x07, 0x09, 0x00, 0x00}
	info := buildUICCAppListInfoForTest([]UICCApplication{
		{Type: 2, AID: usim},
		{Type: 3, AID: isim},
	})

	ft := newFakeTransport()
	ft.reply = func(w []byte) ([]byte, bool) {
		h, _ := decodeHeader(w)
		switch h.Type {
		case MessageTypeOpen:
			return openDoneMsg(h.TransactionID), true
		case MessageTypeCommand:
			if le.Uint32(w[36:]) != CIDUICCApplicationList {
				t.Fatalf("CID = %d, want ApplicationList %d", le.Uint32(w[36:]), CIDUICCApplicationList)
			}
			return makeCommandDoneFragmentFor(h.TransactionID, UUIDMSUICCLowLevelAccess, CIDUICCApplicationList, info), true
		}
		return nil, false
	}
	d := newDevice(ft)
	if err := d.Open(context.Background(), 4096); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	apps, err := QueryUICCApplicationList(context.Background(), d)
	if err != nil {
		t.Fatalf("QueryUICCApplicationList: %v", err)
	}
	if len(apps) != 2 {
		t.Fatalf("apps len = %d, want 2", len(apps))
	}
	if !bytes.Equal(apps[0].AID, usim) {
		t.Fatalf("app[0].AID = %x, want %x", apps[0].AID, usim)
	}
	if !bytes.Equal(apps[1].AID, isim) || apps[1].Type != 3 {
		t.Fatalf("app[1] = %+v, want ISIM type 3", apps[1])
	}
}
