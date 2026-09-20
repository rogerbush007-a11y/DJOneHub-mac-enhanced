package backend

import "testing"

// QMI NAS 的 LTE RSSNR / 5G NR SINR 单位是 0.1 dB 缩放整数，qmiSNRToDB 应四舍五入为 dB 整数。
func TestQMISNRToDB(t *testing.T) {
	cases := []struct {
		raw  int16
		want int
	}{
		{134, 13}, // 13.4 → 13（截图里的异常值，修复前显示 134）
		{135, 14}, // 13.5 → 14（四舍五入进位）
		{0, 0},    // 守卫处已过滤，但保证安全
		{4, 0},    // 0.4 → 0
		{5, 1},    // 0.5 → 1
		{300, 30}, // 30.0 dB
		{-15, -2}, // -1.5 → -2
		{-23, -2}, // -2.3 → -2
	}
	for _, c := range cases {
		if got := qmiSNRToDB(c.raw); got != c.want {
			t.Errorf("qmiSNRToDB(%d)=%d want %d", c.raw, got, c.want)
		}
	}
}
