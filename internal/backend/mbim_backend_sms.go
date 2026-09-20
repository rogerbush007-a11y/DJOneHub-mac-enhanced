package backend

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/iniwex5/vohive/pkg/logger"
	"github.com/iniwex5/vohive/pkg/smscodec"
)

func (b *MBIMBackend) SendSMS(ctx context.Context, to, body string) error {
	return b.SendSMSWithOptions(ctx, to, body, smscodec.SubmitOptions{})
}

func (b *MBIMBackend) SendSMSWithOptions(ctx context.Context, to, body string, opts smscodec.SubmitOptions) error {
	tpdus, _, err := smscodec.BuildSubmitTPDUsWithOptions(to, body, opts)
	if err != nil {
		return fmt.Errorf("PDU 编码失败: %w", err)
	}
	if len(tpdus) == 0 {
		return fmt.Errorf("PDU 编码结果为空")
	}
	for i, tpdu := range tpdus {
		pdu := append([]byte{0x00}, tpdu...)
		if _, err := b.source.SendSMS(ctx, pdu); err != nil {
			return fmt.Errorf("发送第 %d/%d 段失败: %w", i+1, len(tpdus), err)
		}
	}
	logger.Info("MBIM 短信发送成功", "to", redactSMSRecipient(to), "parts", len(tpdus))
	return nil
}

func redactSMSRecipient(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= 4 {
		return "****"
	}
	return "****" + string(runes[len(runes)-4:])
}

func (b *MBIMBackend) ReadSMS(ctx context.Context, index int) (*SMS, error) {
	rec, err := b.source.ReadSMS(ctx, uint32(index))
	if err != nil {
		return nil, err
	}
	return &SMS{Index: index, Content: hex.EncodeToString(rec.PDU)}, nil
}

func (b *MBIMBackend) DeleteSMS(ctx context.Context, index int) error {
	return b.source.DeleteSMS(ctx, uint32(index))
}

func (b *MBIMBackend) ListSMS(ctx context.Context) ([]SMSSummary, error) {
	recs, err := b.source.ListSMS(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SMSSummary, 0, len(recs))
	for _, r := range recs {
		out = append(out, SMSSummary{Index: int(r.Index), Tag: int(r.Status)})
	}
	return out, nil
}

func (b *MBIMBackend) DeleteAllSMS(ctx context.Context) error {
	return b.source.DeleteAllSMS(ctx)
}

func (b *MBIMBackend) GetSMSC(ctx context.Context) (string, error) {
	return b.source.GetSMSC(ctx)
}

func (b *MBIMBackend) SetSMSC(ctx context.Context, smsc string) error {
	return b.source.SetSMSC(ctx, smsc)
}
